package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aungpwint/vibe-agent/store"
)

const ExportVersion = 1

// ExportSession renders a whole session as one JSON document. It carries the
// transcript and the session row, nothing else: there is no participant list to
// export in a single-reader tool.
func (s *Service) ExportSession(ctx context.Context, id ID) (Export, error) {
	session, err := s.Session(ctx, id)
	if err != nil {
		return Export{}, err
	}

	query := `SELECT ` + turnColumns + `
      FROM vibe_turns t
     WHERE t.session_id = $1
     ORDER BY t.sequence
     LIMIT $2`

	turns, err := s.pool.Query(ctx, query, id, maxExportTurns+1)
	if err != nil {
		return Export{}, unavailable(err)
	}
	defer turns.Close()

	exportedTurns := make([]ExportTurn, 0, 16)
	for turns.Next() {
		turn, err := scanTurn(turns)
		if err != nil {
			return Export{}, unavailable(err)
		}
		exportedTurns = append(exportedTurns, ExportTurn{
			Sequence:    turn.Sequence,
			Role:        turn.Role,
			Status:      turn.Status,
			Parts:       turn.Parts,
			Body:        turn.Body,
			Model:       turn.Model,
			AgentID:     turn.AgentID,
			Tokens:      turn.Tokens,
			CreatedAt:   turn.CreatedAt,
			CompletedAt: turn.CompletedAt,
		})
	}
	if err := turns.Err(); err != nil {
		return Export{}, unavailable(err)
	}
	if len(exportedTurns) > maxExportTurns {
		return Export{}, invalid("that session has too many turns to export in one document")
	}

	return Export{
		Version:    ExportVersion,
		ExportedAt: s.now().UTC(),
		Session:    session,
		Turns:      exportedTurns,
	}, nil
}

// ImportSession writes a previously exported session back into the local store
// as a new session. Turn sequences are taken from the document; gaps are
// preserved because the sequence is a turn's identity within its session, not a
// position to renumber.
func (s *Service) ImportSession(ctx context.Context, p ImportSessionParams) (Session, error) {
	title, err := normalizeTitle(p.Title)
	if err != nil {
		return Session{}, err
	}
	if len(p.Turns) > maxExportTurns {
		return Session{}, invalid("that document carries too many turns to import at once")
	}

	agent, err := s.resolveAgent(ctx, p.AgentSlug)
	if err != nil {
		return Session{}, err
	}

	var (
		parentID *ID
		rootID   *ID
		depth    int
	)
	if p.ParentID != nil && !p.ParentID.IsZero() {
		parent, err := s.Session(ctx, *p.ParentID)
		if err != nil {
			return Session{}, err
		}
		if parent.Status != SessionOpen {
			return Session{}, conflict("a closed session cannot take an imported child session")
		}
		if agent.Mode == ModePrimary {
			return Session{}, invalid("a child session needs a subagent, not a primary agent")
		}
		depth = parent.Depth + 1
		if depth > maxSessionDepth {
			return Session{}, invalid("sessions cannot nest more than 32 levels deep")
		}
		value := parent.ID
		parentID = &value
		if parent.RootID != nil {
			root := *parent.RootID
			rootID = &root
		} else {
			rootID = &value
		}
	}

	permissions, err := NormalizeRuleset(p.Permissions)
	if err != nil {
		return Session{}, err
	}
	metadata, err := normalizeMetadata(p.Metadata)
	if err != nil {
		return Session{}, err
	}
	model, err := optionalText(p.Model, maxModelLength, "model")
	if err != nil {
		return Session{}, err
	}
	workspace, err := optionalText(p.Workspace, maxPathLength, "workspace")
	if err != nil {
		return Session{}, err
	}
	project, err := optionalText(p.Project, maxPathLength, "project")
	if err != nil {
		return Session{}, err
	}

	type normalizedTurn struct {
		sequence    int64
		role        TurnRole
		status      TurnStatus
		parts       []byte
		body        *string
		model       *string
		tokens      Tokens
		createdAt   time.Time
		completedAt *time.Time
	}

	normalized := make([]normalizedTurn, 0, len(p.Turns))
	var (
		lastSequence int64
		totals       Tokens
	)
	for _, turn := range p.Turns {
		if turn.Sequence <= lastSequence {
			return Session{}, invalid("imported turns must carry increasing, unique sequence numbers")
		}
		if !turn.Role.valid() || !turn.Status.valid() {
			return Session{}, invalid("an imported turn carries an unknown role or state")
		}
		if err := validateTokens(turn.Tokens); err != nil {
			return Session{}, err
		}
		parts, body, err := NormalizeParts(turn.Parts)
		if err != nil {
			return Session{}, err
		}
		rawParts, err := json.Marshal(parts)
		if err != nil {
			return Session{}, unavailable(err)
		}

		createdAt := turn.CreatedAt
		if createdAt.IsZero() {
			createdAt = s.now()
		}
		completedAt := turn.CompletedAt
		if completedAt == nil && turn.Status != TurnStreaming {
			value := createdAt
			completedAt = &value
		}

		normalized = append(normalized, normalizedTurn{
			sequence:    turn.Sequence,
			role:        turn.Role,
			status:      turn.Status,
			parts:       rawParts,
			body:        nullableBody(body),
			model:       turn.Model,
			tokens:      turn.Tokens,
			createdAt:   createdAt,
			completedAt: completedAt,
		})
		lastSequence = turn.Sequence
		totals.Prompt += turn.Tokens.Prompt
		totals.Completion += turn.Tokens.Completion
		totals.CostMicros += turn.Tokens.CostMicros
	}

	now := s.now()
	id := NewID(now)

	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		return Session{}, unavailable(err)
	}
	rawPermissions, err := json.Marshal(permissions)
	if err != nil {
		return Session{}, unavailable(err)
	}

	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		const insertSession = `
    INSERT INTO vibe_sessions
        (id, parent_id, root_id, depth, title, agent_id, agent_slug, model,
         workspace, project, metadata, permissions, status, last_activity_at, created_at, updated_at)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'open', $13, $13, $13)`

		if _, err := tx.Exec(ctx, insertSession, id, parentID, rootID, depth, title,
			agent.ID, agent.Slug, model, workspace, project, rawMetadata, rawPermissions, now); err != nil {
			return err
		}

		const insertTurn = `
    INSERT INTO vibe_turns
        (id, session_id, sequence, role, status, parts, body, model,
         prompt_tokens, completion_tokens, cost_micros, created_at, completed_at)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

		for _, turn := range normalized {
			if _, err := tx.Exec(ctx, insertTurn, NewID(now), id, turn.sequence,
				string(turn.role), string(turn.status), turn.parts, turn.body,
				turn.model, turn.tokens.Prompt, turn.tokens.Completion, turn.tokens.CostMicros,
				turn.createdAt, turn.completedAt); err != nil {
				return err
			}
		}

		const finish = `
    UPDATE vibe_sessions
       SET next_sequence = $2, turn_count = $3,
           prompt_tokens = $4, completion_tokens = $5, cost_micros = $6
     WHERE id = $1`

		_, err := tx.Exec(ctx, finish, id, lastSequence, len(normalized),
			totals.Prompt, totals.Completion, totals.CostMicros)
		return err
	})
	if err != nil {
		switch {
		case store.IsUniqueViolation(err):
			return Session{}, invalid("that document names the same turn twice")
		case store.IsForeignKeyViolation(err), store.IsCheckViolation(err):
			return Session{}, invalid("that document cannot be imported as it is")
		default:
			return Session{}, unavailable(err)
		}
	}

	return s.Session(ctx, id)
}
