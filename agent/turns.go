package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/aungpwint/vibe-agent/store"
)

const turnColumns = `
    t.id, t.session_id, t.sequence, t.role, t.status, t.parent_turn_id,
    t.parts, t.body, t.model, t.agent_id, t.run_id, t.prompt_tokens, t.completion_tokens,
    t.cost_micros, t.client_id, t.redacted_at, t.created_at, t.completed_at`

func scanTurn(row rowScanner) (Turn, error) {
	var (
		t     Turn
		role  string
		state string
		parts []byte
	)
	if err := row.Scan(&t.ID, &t.SessionID, &t.Sequence, &role, &state,
		&t.ParentTurnID, &parts, &t.Body, &t.Model, &t.AgentID, &t.RunID,
		&t.Tokens.Prompt, &t.Tokens.Completion, &t.Tokens.CostMicros, &t.ClientID,
		&t.RedactedAt, &t.CreatedAt, &t.CompletedAt); err != nil {
		return Turn{}, err
	}

	t.Role = TurnRole(role)
	t.Status = TurnStatus(state)

	var err error
	if t.Parts, err = decodeParts(parts); err != nil {
		return Turn{}, err
	}
	return t, nil
}

func (s *Service) AppendTurn(ctx context.Context, p AppendTurnParams) (Turn, error) {
	role := p.Role
	if role == "" {
		role = TurnUser
	}
	if !role.valid() {
		return Turn{}, invalid("that is not a role a turn can have")
	}
	status := p.Status
	if status == "" {
		status = TurnComplete
	}
	if !status.valid() {
		return Turn{}, invalid("that is not a state a turn can be in")
	}
	if err := validateTokens(p.Tokens); err != nil {
		return Turn{}, err
	}

	clientID, err := normalizeClientID(p.ClientID)
	if err != nil {
		return Turn{}, err
	}

	parts, body, err := NormalizeParts(p.Parts)
	if err != nil {
		return Turn{}, err
	}
	if len(parts) == 0 {
		return Turn{}, invalid("a turn needs at least one part")
	}

	current, err := s.Session(ctx, p.SessionID)
	if err != nil {
		return Turn{}, err
	}
	if current.Status != SessionOpen {
		return Turn{}, conflict("that session is closed and cannot take new turns")
	}

	model, err := optionalText(p.Model, maxModelLength, "model")
	if err != nil {
		return Turn{}, err
	}
	if model == nil {
		model = current.Model
	}

	if p.RunID != nil {
		if err := s.liveRunBelongs(ctx, *p.RunID, p.SessionID); err != nil {
			return Turn{}, err
		}
	}
	if p.ParentTurnID != nil {
		if err := s.turnBelongs(ctx, *p.ParentTurnID, p.SessionID); err != nil {
			return Turn{}, err
		}
	}

	// A retried append is answered with the turn that already exists. The
	// lookup here is the cheap path; the unique index below is the arbiter when
	// two retries race.
	if clientID != nil {
		existing, err := s.turnByClientID(ctx, p.SessionID, *clientID)
		switch {
		case err == nil:
			return existing, nil
		case !errors.Is(err, ErrTurnNotFound):
			return Turn{}, err
		}
	}

	now := s.now()
	turnID := NewID(now)

	rawParts, err := json.Marshal(parts)
	if err != nil {
		return Turn{}, unavailable(err)
	}

	var stored Turn
	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		sequence, err := allocateSequence(ctx, tx, p.SessionID, now)
		if err != nil {
			if errors.Is(err, ErrClosed) {
				return conflict("that session is closed and cannot take new turns")
			}
			return err
		}

		const insert = `
    INSERT INTO vibe_turns AS t
        (id, session_id, sequence, role, status, parent_turn_id, parts, body,
         model, agent_id, run_id, prompt_tokens, completion_tokens, cost_micros, client_id,
         created_at, completed_at)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
    ON CONFLICT (session_id, client_id) WHERE client_id IS NOT NULL DO NOTHING
    RETURNING ` + turnColumns

		completedAt := any(nil)
		if status != TurnStreaming {
			completedAt = now
		}

		stored, err = scanTurn(tx.QueryRow(ctx, insert, turnID, p.SessionID, sequence,
			string(role), string(status), p.ParentTurnID, rawParts, nullableBody(body), model,
			p.AgentID, p.RunID, p.Tokens.Prompt, p.Tokens.Completion, p.Tokens.CostMicros,
			clientID, now, completedAt))
		if store.IsNoRows(err) {
			if clientID == nil {
				return errors.New("agent: insert returned no row without a client id to conflict on")
			}
			existing, err := s.turnByClientIDTx(ctx, tx, p.SessionID, *clientID)
			if err != nil {
				return err
			}
			const compensate = `
    UPDATE vibe_sessions
       SET next_sequence = GREATEST(next_sequence - 1, 0),
           turn_count = GREATEST(turn_count - 1, 0)
     WHERE id = $1`
			if _, err := tx.Exec(ctx, compensate, p.SessionID); err != nil {
				return err
			}
			stored = existing
			return nil
		}
		if err != nil {
			return err
		}

		if p.Tokens.Prompt != 0 || p.Tokens.Completion != 0 || p.Tokens.CostMicros != 0 {
			const totals = `
    UPDATE vibe_sessions
       SET prompt_tokens = prompt_tokens + $2,
           completion_tokens = completion_tokens + $3,
           cost_micros = cost_micros + $4
     WHERE id = $1`

			if _, err := tx.Exec(ctx, totals, p.SessionID,
				p.Tokens.Prompt, p.Tokens.Completion, p.Tokens.CostMicros); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		var domainErr *Error
		if errors.As(err, &domainErr) {
			return Turn{}, err
		}
		switch {
		case store.IsCheckViolation(err), store.IsForeignKeyViolation(err):
			return Turn{}, invalid("that turn cannot be appended as it is")
		case store.IsUniqueViolation(err):
			return Turn{}, conflict("that turn was already appended")
		default:
			return Turn{}, unavailable(err)
		}
	}

	return stored, nil
}

func (s *Service) CompleteTurn(ctx context.Context, p CompleteTurnParams) (Turn, error) {
	if !p.Status.valid() {
		return Turn{}, invalid("that is not a state a turn can be in")
	}

	current, err := s.turn(ctx, p.ID)
	if err != nil {
		return Turn{}, err
	}

	if current.Status != TurnStreaming {
		if current.Status == p.Status {
			return current, nil
		}
		return Turn{}, conflict("that turn has already finished in another state")
	}

	tokens := current.Tokens
	if p.Tokens != nil {
		if err := validateTokens(*p.Tokens); err != nil {
			return Turn{}, err
		}
		tokens = *p.Tokens
	}

	parts := current.Parts
	body := current.Body
	if p.Parts != nil {
		normalized, derived, err := NormalizeParts(p.Parts)
		if err != nil {
			return Turn{}, err
		}
		parts = normalized
		body = nullableBody(derived)
	}

	delta := tokens.delta(current.Tokens)
	if delta.Prompt < 0 || delta.Completion < 0 || delta.CostMicros < 0 {
		return Turn{}, invalid("a turn's token totals cannot go backwards")
	}

	rawParts, err := json.Marshal(parts)
	if err != nil {
		return Turn{}, unavailable(err)
	}

	now := s.now()
	var updated Turn

	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		const update = `
    UPDATE vibe_turns AS t
       SET status = $2, parts = $3, body = $4,
           prompt_tokens = $5, completion_tokens = $6, cost_micros = $7,
           completed_at = coalesce(completed_at, $8)
     WHERE id = $1 AND status = 'streaming'
    RETURNING ` + turnColumns

		stored, err := scanTurn(tx.QueryRow(ctx, update, p.ID, string(p.Status), rawParts, body,
			tokens.Prompt, tokens.Completion, tokens.CostMicros, now))
		if store.IsNoRows(err) {
			existing, findErr := s.turnTx(ctx, tx, p.ID)
			if findErr != nil {
				return findErr
			}
			if existing.Status == p.Status {
				updated = existing
				return nil
			}
			return conflict("that turn has already finished in another state")
		}
		if err != nil {
			return err
		}

		if delta.Prompt != 0 || delta.Completion != 0 || delta.CostMicros != 0 {
			const totals = `
    UPDATE vibe_sessions
       SET prompt_tokens = prompt_tokens + $2,
           completion_tokens = completion_tokens + $3,
           cost_micros = cost_micros + $4
     WHERE id = $1`

			if _, err := tx.Exec(ctx, totals, stored.SessionID,
				delta.Prompt, delta.Completion, delta.CostMicros); err != nil {
				return err
			}
		}

		updated = stored
		return nil
	})
	if err != nil {
		var domainErr *Error
		if errors.As(err, &domainErr) {
			return Turn{}, err
		}
		return Turn{}, unavailable(err)
	}

	return updated, nil
}

// RedactTurn withdraws the content of a turn but keeps its place in the
// transcript: the sequence, tokens and timestamps stay so the history still
// adds up, while parts and body are emptied.
func (s *Service) RedactTurn(ctx context.Context, id ID) (Turn, error) {
	current, err := s.turn(ctx, id)
	if err != nil {
		return Turn{}, err
	}
	if current.IsRedacted() {
		return current, nil
	}

	now := s.now()

	const update = `
    UPDATE vibe_turns AS t
       SET parts = '[]'::jsonb, body = NULL, redacted_at = $2
     WHERE id = $1
    RETURNING ` + turnColumns

	redacted, err := scanTurn(s.pool.QueryRow(ctx, update, id, now))
	if err != nil {
		return Turn{}, unavailable(err)
	}
	return redacted, nil
}

func (s *Service) Turns(ctx context.Context, q TurnQuery) (TurnPage, error) {
	if q.AfterSequence < 0 {
		return TurnPage{}, invalid("a turn cursor cannot be negative")
	}
	if q.Role != nil && !q.Role.valid() {
		return TurnPage{}, invalid("that is not a role a turn can have")
	}
	if _, err := s.Session(ctx, q.SessionID); err != nil {
		return TurnPage{}, err
	}

	limit := normalizeLimit(q.Limit)

	var role *string
	if q.Role != nil {
		value := string(*q.Role)
		role = &value
	}

	const query = `
    SELECT ` + turnColumns + `
      FROM vibe_turns t
     WHERE t.session_id = $1
       AND t.sequence > $2
       AND ($3::text IS NULL OR t.role = $3)
     ORDER BY t.sequence
     LIMIT $4`

	rows, err := s.pool.Query(ctx, query, q.SessionID, q.AfterSequence, role, limit+1)
	if err != nil {
		return TurnPage{}, unavailable(err)
	}
	defer rows.Close()

	page := TurnPage{NextSeq: q.AfterSequence}
	for rows.Next() {
		turn, err := scanTurn(rows)
		if err != nil {
			return TurnPage{}, unavailable(err)
		}
		page.Items = append(page.Items, turn)
	}
	if err := rows.Err(); err != nil {
		return TurnPage{}, unavailable(err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
	}
	if n := len(page.Items); n > 0 {
		page.NextSeq = page.Items[n-1].Sequence
	}

	total, err := s.turnCount(ctx, q.SessionID, role)
	if err != nil {
		return TurnPage{}, err
	}
	page.TotalCount = &total
	return page, nil
}

func (s *Service) SearchTurns(ctx context.Context, q TurnSearch) (TurnPage, error) {
	needle := strings.TrimSpace(q.Text)
	if utf8.RuneCountInString(needle) < maxSearchNeedleRunes {
		empty := 0
		return TurnPage{Items: []Turn{}, TotalCount: &empty}, nil
	}
	if q.Role != nil && !q.Role.valid() {
		return TurnPage{}, invalid("that is not a role a turn can have")
	}

	limit := normalizeLimit(q.Limit)

	var role *string
	if q.Role != nil {
		value := string(*q.Role)
		role = &value
	}

	const query = `
    SELECT ` + turnColumns + `
      FROM vibe_turns t
      JOIN vibe_sessions s ON s.id = t.session_id
     WHERE t.redacted_at IS NULL
       AND lower(coalesce(t.body, '')) %> lower($1)
       AND ($2::char(26) IS NULL OR t.session_id = $2)
       AND ($3::text IS NULL OR t.role = $3)
       AND ($4::text IS NULL OR s.agent_slug = $4)
     ORDER BY t.created_at DESC, t.id DESC
     LIMIT $5`

	rows, err := s.pool.Query(ctx, query, needle, q.SessionID, role, q.AgentSlug, limit+1)
	if err != nil {
		return TurnPage{}, unavailable(err)
	}
	defer rows.Close()

	page := TurnPage{}
	for rows.Next() {
		turn, err := scanTurn(rows)
		if err != nil {
			return TurnPage{}, unavailable(err)
		}
		page.Items = append(page.Items, turn)
	}
	if err := rows.Err(); err != nil {
		return TurnPage{}, unavailable(err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
	}
	return page, nil
}

func (s *Service) turn(ctx context.Context, id ID) (Turn, error) {
	const query = `SELECT ` + turnColumns + ` FROM vibe_turns t WHERE t.id = $1`
	return s.scanTurnRow(s.pool.QueryRow(ctx, query, id))
}

func (s *Service) turnTx(ctx context.Context, q store.Querier, id ID) (Turn, error) {
	const query = `SELECT ` + turnColumns + ` FROM vibe_turns t WHERE t.id = $1`
	return s.scanTurnRow(q.QueryRow(ctx, query, id))
}

func (s *Service) scanTurnRow(row pgx.Row) (Turn, error) {
	turn, err := scanTurn(row)
	if err != nil {
		if store.IsNoRows(err) {
			return Turn{}, ErrTurnNotFound
		}
		return Turn{}, unavailable(err)
	}
	return turn, nil
}

func (s *Service) turnByClientID(ctx context.Context, sessionID ID, clientID string) (Turn, error) {
	const query = `
    SELECT ` + turnColumns + `
      FROM vibe_turns t
     WHERE t.session_id = $1 AND t.client_id = $2`

	return s.scanTurnRow(s.pool.QueryRow(ctx, query, sessionID, clientID))
}

func (s *Service) turnByClientIDTx(ctx context.Context, q store.Querier, sessionID ID, clientID string) (Turn, error) {
	const query = `
    SELECT ` + turnColumns + `
      FROM vibe_turns t
     WHERE t.session_id = $1 AND t.client_id = $2`

	return s.scanTurnRow(q.QueryRow(ctx, query, sessionID, clientID))
}

func (s *Service) turnBelongs(ctx context.Context, id, sessionID ID) error {
	const query = `SELECT EXISTS (SELECT 1 FROM vibe_turns WHERE id = $1 AND session_id = $2)`

	var found bool
	if err := s.pool.QueryRow(ctx, query, id, sessionID).Scan(&found); err != nil {
		return unavailable(err)
	}
	if !found {
		return invalid("the turn being replied to is not in this session")
	}
	return nil
}

func (s *Service) liveRunBelongs(ctx context.Context, id, sessionID ID) error {
	const query = `
    SELECT EXISTS (SELECT 1 FROM vibe_runs
                    WHERE id = $1 AND session_id = $2
                      AND status IN ('queued', 'running'))`

	var found bool
	if err := s.pool.QueryRow(ctx, query, id, sessionID).Scan(&found); err != nil {
		return unavailable(err)
	}
	if !found {
		return invalid("that run is not live in this session")
	}
	return nil
}

func (s *Service) turnCount(ctx context.Context, sessionID ID, role *string) (int, error) {
	const query = `
    SELECT count(*)
      FROM vibe_turns
     WHERE session_id = $1 AND ($2::text IS NULL OR role = $2)`

	var total int
	if err := s.pool.QueryRow(ctx, query, sessionID, role).Scan(&total); err != nil {
		return 0, unavailable(err)
	}
	return total, nil
}
