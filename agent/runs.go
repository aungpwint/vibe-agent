package agent

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/aungpwint/vibe-agent/store"
)

const runColumns = `
    r.id, r.session_id, r.agent_id, r.status, r.model, r.child_session_id,
    r.steps, r.prompt_tokens, r.completion_tokens, r.cost_micros, r.error,
    r.started_at, r.finished_at, r.created_at`

func scanRun(row rowScanner) (Run, error) {
	var (
		r      Run
		status string
	)
	if err := row.Scan(&r.ID, &r.SessionID, &r.AgentID, &status, &r.Model,
		&r.ChildSessionID, &r.Steps, &r.Tokens.Prompt, &r.Tokens.Completion,
		&r.Tokens.CostMicros, &r.Error, &r.StartedAt, &r.FinishedAt,
		&r.CreatedAt); err != nil {
		return Run{}, err
	}
	r.Status = RunStatus(status)
	return r, nil
}

func (s *Service) StartRun(ctx context.Context, p StartRunParams) (Run, error) {
	session, err := s.Session(ctx, p.SessionID)
	if err != nil {
		return Run{}, err
	}
	if session.Status != SessionOpen {
		return Run{}, conflict("a closed session cannot start a run")
	}

	model, err := optionalText(p.Model, maxModelLength, "model")
	if err != nil {
		return Run{}, err
	}
	if model == nil {
		model = session.Model
	}

	agentID := p.AgentID
	if agentID == nil {
		value := session.AgentID
		agentID = &value
	}

	now := s.now()
	id := NewID(now)

	const insert = `
    INSERT INTO vibe_runs AS r
        (id, session_id, agent_id, status, model, started_at, created_at)
    VALUES ($1, $2, $3, 'running', $4, $5, $5)
    RETURNING ` + runColumns

	run, err := scanRun(s.pool.QueryRow(ctx, insert, id, p.SessionID, agentID, model, now))
	if err != nil {
		switch {
		case store.IsUniqueViolation(err):
			return Run{}, conflict("a run is already active in that session")
		case store.IsForeignKeyViolation(err), store.IsCheckViolation(err):
			return Run{}, invalid("that run cannot be started as it is")
		default:
			return Run{}, unavailable(err)
		}
	}
	return run, nil
}

func (s *Service) FinishRun(ctx context.Context, p FinishRunParams) (Run, error) {
	if !p.Status.Terminal() {
		return Run{}, invalid("a run can only finish as completed, failed or cancelled")
	}
	if p.Steps < 0 {
		return Run{}, invalid("a run cannot take negative steps")
	}
	if err := validateTokens(p.Tokens); err != nil {
		return Run{}, err
	}

	current, err := s.run(ctx, p.ID)
	if err != nil {
		return Run{}, err
	}
	if current.Status.Terminal() {
		if current.Status == p.Status {
			return current, nil
		}
		return Run{}, conflict("that run has already finished in another state")
	}

	message, err := optionalText(p.Error, 8000, "error")
	if err != nil {
		return Run{}, err
	}
	if p.Status != RunFailed {
		message = nil
	}

	if p.ChildSessionID != nil {
		if err := s.childBelongs(ctx, *p.ChildSessionID, current.SessionID); err != nil {
			return Run{}, err
		}
	}

	now := s.now()
	var run Run

	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		const update = `
    UPDATE vibe_runs AS r
       SET status = $2, steps = $3, prompt_tokens = $4, completion_tokens = $5,
           cost_micros = $6, error = $7, child_session_id = $8, finished_at = $9
     WHERE id = $1 AND status IN ('queued', 'running')
    RETURNING ` + runColumns

		stored, err := scanRun(tx.QueryRow(ctx, update, p.ID, string(p.Status), p.Steps,
			p.Tokens.Prompt, p.Tokens.Completion, p.Tokens.CostMicros, message,
			p.ChildSessionID, now))
		if store.IsNoRows(err) {
			existing, findErr := s.runTx(ctx, tx, p.ID)
			if findErr != nil {
				return findErr
			}
			if existing.Status == p.Status {
				run = existing
				return nil
			}
			return conflict("that run has already finished in another state")
		}
		if err != nil {
			return err
		}

		run = stored
		return nil
	})
	if err != nil {
		var domainErr *Error
		if errors.As(err, &domainErr) {
			return Run{}, err
		}
		return Run{}, unavailable(err)
	}
	return run, nil
}

func (s *Service) CancelRun(ctx context.Context, id ID) (Run, error) {
	current, err := s.run(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if current.Status.Terminal() {
		return current, nil
	}

	now := s.now()
	var run Run

	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		const update = `
    UPDATE vibe_runs AS r
       SET status = 'cancelled', finished_at = $2
     WHERE id = $1 AND status IN ('queued', 'running')
    RETURNING ` + runColumns

		run, err = scanRun(tx.QueryRow(ctx, update, id, now))
		if store.IsNoRows(err) {
			existing, findErr := s.runTx(ctx, tx, id)
			if findErr != nil {
				return findErr
			}
			run = existing
			return nil
		}
		return err
	})
	if err != nil {
		return Run{}, unavailable(err)
	}
	return run, nil
}

func (s *Service) Runs(ctx context.Context, sessionID ID, limit int) ([]Run, error) {
	if _, err := s.Session(ctx, sessionID); err != nil {
		return nil, err
	}
	limit = normalizeLimit(limit)

	const query = `
    SELECT ` + runColumns + `
      FROM vibe_runs r
     WHERE r.session_id = $1
     ORDER BY r.created_at DESC, r.id DESC
     LIMIT $2`

	return s.queryRuns(ctx, query, sessionID, limit)
}

func (s *Service) ActiveRuns(ctx context.Context, sessionID ID) ([]Run, error) {
	if _, err := s.Session(ctx, sessionID); err != nil {
		return nil, err
	}

	const query = `
    SELECT ` + runColumns + `
      FROM vibe_runs r
     WHERE r.session_id = $1 AND r.status IN ('queued', 'running')
     ORDER BY r.created_at, r.id`

	return s.queryRuns(ctx, query, sessionID)
}

func (s *Service) RunStats(ctx context.Context, sessionID ID) (RunStats, error) {
	if _, err := s.Session(ctx, sessionID); err != nil {
		return RunStats{}, err
	}

	const query = `
    SELECT count(*),
           count(*) FILTER (WHERE status = 'queued'),
           count(*) FILTER (WHERE status = 'running'),
           count(*) FILTER (WHERE status = 'completed'),
           count(*) FILTER (WHERE status = 'failed'),
           count(*) FILTER (WHERE status = 'cancelled'),
           coalesce(sum(steps), 0),
           coalesce(sum(prompt_tokens), 0),
           coalesce(sum(completion_tokens), 0),
           coalesce(sum(cost_micros), 0)
      FROM vibe_runs
     WHERE session_id = $1`

	var stats RunStats
	if err := s.pool.QueryRow(ctx, query, sessionID).Scan(
		&stats.Total, &stats.Queued, &stats.Running, &stats.Completed, &stats.Failed,
		&stats.Cancelled, &stats.Steps, &stats.Tokens.Prompt, &stats.Tokens.Completion,
		&stats.Tokens.CostMicros,
	); err != nil {
		return RunStats{}, unavailable(err)
	}
	return stats, nil
}

func (s *Service) queryRuns(ctx context.Context, query string, args ...any) ([]Run, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()

	out := make([]Run, 0, 4)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

func (s *Service) run(ctx context.Context, id ID) (Run, error) {
	const query = `SELECT ` + runColumns + ` FROM vibe_runs r WHERE r.id = $1`
	return s.scanRunRow(s.pool.QueryRow(ctx, query, id))
}

func (s *Service) runTx(ctx context.Context, q store.Querier, id ID) (Run, error) {
	const query = `SELECT ` + runColumns + ` FROM vibe_runs r WHERE r.id = $1`
	return s.scanRunRow(q.QueryRow(ctx, query, id))
}

func (s *Service) scanRunRow(row pgx.Row) (Run, error) {
	run, err := scanRun(row)
	if err != nil {
		if store.IsNoRows(err) {
			return Run{}, ErrRunNotFound
		}
		return Run{}, unavailable(err)
	}
	return run, nil
}

func (s *Service) childBelongs(ctx context.Context, childID, parentID ID) error {
	const query = `
    SELECT EXISTS (SELECT 1 FROM vibe_sessions
                    WHERE id = $1 AND parent_id = $2)`

	var found bool
	if err := s.pool.QueryRow(ctx, query, childID, parentID).Scan(&found); err != nil {
		return unavailable(err)
	}
	if !found {
		return invalid("that session is not a child of the session this run belongs to")
	}
	return nil
}
