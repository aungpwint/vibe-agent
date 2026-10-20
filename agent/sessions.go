package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aungpwint/vibe-agent/store"
)

const sessionColumns = `
    s.id, s.parent_id, s.root_id, s.depth, s.title, s.agent_id, s.agent_slug,
    s.model, s.workspace, s.project, s.metadata, s.permissions, s.status,
    s.next_sequence, s.turn_count, s.prompt_tokens, s.completion_tokens, s.cost_micros,
    s.forked_from_id, s.forked_from_sequence, s.archived_at, s.last_activity_at, s.closed_at,
    s.created_at, s.updated_at`

func scanSession(row rowScanner) (Session, error) {
	var (
		s           Session
		metadata    []byte
		permissions []byte
		status      string
	)
	if err := row.Scan(&s.ID, &s.ParentID, &s.RootID, &s.Depth, &s.Title,
		&s.AgentID, &s.AgentSlug, &s.Model, &s.Workspace, &s.Project, &metadata,
		&permissions, &status, &s.NextSequence, &s.TurnCount,
		&s.Tokens.Prompt, &s.Tokens.Completion, &s.Tokens.CostMicros,
		&s.ForkedFromID, &s.ForkedFromSequence, &s.ArchivedAt, &s.LastActivityAt,
		&s.ClosedAt, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return Session{}, err
	}

	s.Status = SessionStatus(status)

	var err error
	if s.Metadata, err = decodeMetadata(metadata); err != nil {
		return Session{}, err
	}
	if s.Permissions, err = decodeRuleset(permissions); err != nil {
		return Session{}, err
	}
	return s, nil
}

func (s *Service) Session(ctx context.Context, id ID) (Session, error) {
	const query = `SELECT ` + sessionColumns + ` FROM vibe_sessions s WHERE s.id = $1`

	session, err := scanSession(s.pool.QueryRow(ctx, query, id))
	if err != nil {
		return Session{}, sessionError(err)
	}
	return session, nil
}

// Sessions lists the tree newest activity first. The archive filter decides
// whether the default list, the archived list or everything is read.
func (s *Service) Sessions(ctx context.Context, limit int, q SessionQuery) (SessionPage, error) {
	if !q.Archived.valid() {
		return SessionPage{}, invalid("unknown archive filter %q", q.Archived)
	}
	if q.Archived == "" {
		q.Archived = ActiveOnly
	}
	limit = normalizeLimit(limit)

	var status *string
	if q.Status != nil {
		if !q.Status.valid() {
			return SessionPage{}, invalid("that is not a state a session can be in")
		}
		value := string(*q.Status)
		status = &value
	}

	const query = `
    SELECT ` + sessionColumns + `
      FROM vibe_sessions s
     WHERE ($1::text = 'all'
            OR ($1::text = 'active' AND s.archived_at IS NULL)
            OR ($1::text = 'archived' AND s.archived_at IS NOT NULL))
       AND ($2::char(26) IS NULL OR s.parent_id = $2)
       AND ($3::char(26) IS NULL OR s.root_id = $3)
       AND ($4::text IS NULL OR s.status = $4)
       AND ($5::text IS NULL OR s.agent_slug = $5)
     ORDER BY s.last_activity_at DESC, s.id DESC
     LIMIT $6`

	rows, err := s.pool.Query(ctx, query, string(q.Archived), q.ParentID, q.RootID,
		status, q.AgentSlug, limit+1)
	if err != nil {
		return SessionPage{}, unavailable(err)
	}
	defer rows.Close()

	page := SessionPage{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return SessionPage{}, unavailable(err)
		}
		page.Items = append(page.Items, session)
	}
	if err := rows.Err(); err != nil {
		return SessionPage{}, unavailable(err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasNext = true
	}
	return page, nil
}

func (s *Service) CreateSession(ctx context.Context, p CreateSessionParams) (Session, error) {
	title, err := normalizeTitle(p.Title)
	if err != nil {
		return Session{}, err
	}

	agent, err := s.resolveAgent(ctx, p.AgentSlug)
	if err != nil {
		return Session{}, err
	}

	var (
		parent   Session
		parentID *ID
		rootID   *ID
		depth    int
	)
	if p.ParentID != nil && !p.ParentID.IsZero() {
		parent, err = s.Session(ctx, *p.ParentID)
		if err != nil {
			return Session{}, err
		}
		if parent.Status != SessionOpen {
			return Session{}, conflict("a closed session cannot start a child session")
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

	permissions := agent.Permissions
	if p.Permissions != nil {
		permissions, err = NormalizeRuleset(*p.Permissions)
		if err != nil {
			return Session{}, err
		}
	}
	metadata, err := normalizeMetadata(p.Metadata)
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
	model := p.Model
	if model == nil {
		model = agent.Model
	}
	model, err = optionalText(model, maxModelLength, "model")
	if err != nil {
		return Session{}, err
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

		_, err := tx.Exec(ctx, insertSession, id, parentID, rootID, depth, title,
			agent.ID, agent.Slug, model, workspace, project, rawMetadata, rawPermissions, now)
		return err
	})
	if err != nil {
		switch {
		case store.IsForeignKeyViolation(err), store.IsCheckViolation(err):
			return Session{}, invalid("that session cannot be created as it is")
		case store.IsUniqueViolation(err):
			return Session{}, conflict("that session already exists")
		default:
			return Session{}, unavailable(err)
		}
	}

	return s.Session(ctx, id)
}

func (s *Service) UpdateSession(ctx context.Context, p UpdateSessionParams) (Session, error) {
	var updated Session

	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		const query = `SELECT ` + sessionColumns + ` FROM vibe_sessions s WHERE s.id = $1 FOR UPDATE OF s`

		current, err := scanSession(tx.QueryRow(ctx, query, p.ID))
		if err != nil {
			return sessionError(err)
		}
		if current.Status != SessionOpen {
			return conflict("a closed session cannot be changed until it is reopened")
		}

		if p.Title != nil {
			current.Title, err = normalizeTitle(*p.Title)
			if err != nil {
				return err
			}
		}
		if p.Model != nil {
			next, err := optionalText(p.Model, maxModelLength, "model")
			if err != nil {
				return err
			}
			if (next == nil) != (current.Model == nil) || (next != nil && *next != *current.Model) {
				if next != nil {
					if err := s.appendSystemPart(ctx, tx, current.ID, Part{Type: "model", Model: *next}, s.now()); err != nil {
						return err
					}
				}
			}
			current.Model = next
		}
		if p.Metadata != nil {
			current.Metadata, err = normalizeMetadata(*p.Metadata)
			if err != nil {
				return err
			}
		}
		if p.Permissions != nil {
			current.Permissions, err = NormalizeRuleset(*p.Permissions)
			if err != nil {
				return err
			}
		}

		rawMetadata, err := json.Marshal(current.Metadata)
		if err != nil {
			return err
		}
		rawPermissions, err := json.Marshal(current.Permissions)
		if err != nil {
			return err
		}

		const update = `
    UPDATE vibe_sessions
       SET title = $2, model = $3, metadata = $4, permissions = $5, updated_at = now()
     WHERE id = $1`

		if _, err := tx.Exec(ctx, update, p.ID, current.Title, current.Model, rawMetadata, rawPermissions); err != nil {
			return err
		}

		updated, err = scanSession(tx.QueryRow(ctx, query, p.ID))
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return updated, nil
}

func (s *Service) CloseSession(ctx context.Context, id ID) (Session, error) {
	return s.setSessionStatus(ctx, id, SessionClosed)
}

func (s *Service) ReopenSession(ctx context.Context, id ID) (Session, error) {
	return s.setSessionStatus(ctx, id, SessionOpen)
}

func (s *Service) setSessionStatus(ctx context.Context, id ID, status SessionStatus) (Session, error) {
	var updated Session

	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		const query = `SELECT ` + sessionColumns + ` FROM vibe_sessions s WHERE s.id = $1 FOR UPDATE OF s`

		current, err := scanSession(tx.QueryRow(ctx, query, id))
		if err != nil {
			return sessionError(err)
		}

		if current.Status != status {
			const update = `
    UPDATE vibe_sessions
       SET status = $2,
           closed_at = CASE WHEN $2 = 'closed' THEN coalesce(closed_at, $3) ELSE NULL END,
           updated_at = now()
     WHERE id = $1`

			if _, err := tx.Exec(ctx, update, id, string(status), s.now()); err != nil {
				return err
			}
		}

		updated, err = scanSession(tx.QueryRow(ctx, query, id))
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return updated, nil
}

func (s *Service) DeleteSession(ctx context.Context, id ID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM vibe_sessions WHERE id = $1`, id)
	if err != nil {
		return unavailable(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// SetSessionArchived hides or restores a session in the default list. Archiving
// is a filing decision, not a state transition: a closed session can be
// archived and reopened afterwards without leaving the archive first.
func (s *Service) SetSessionArchived(ctx context.Context, id ID, archived bool) (Session, error) {
	const update = `
    UPDATE vibe_sessions s
       SET archived_at = CASE WHEN $2 THEN coalesce(s.archived_at, now()) ELSE NULL END,
           updated_at = now()
     WHERE s.id = $1
    RETURNING ` + sessionColumns

	session, err := scanSession(s.pool.QueryRow(ctx, update, id, archived))
	if err != nil {
		return Session{}, sessionError(err)
	}
	return session, nil
}

func (s *Service) ForkSession(ctx context.Context, p ForkSessionParams) (Session, error) {
	source, err := s.Session(ctx, p.ID)
	if err != nil {
		return Session{}, err
	}

	at := source.NextSequence
	if p.AtSequence != nil {
		if *p.AtSequence < 0 || *p.AtSequence > source.NextSequence {
			return Session{}, invalid("a fork must point at or before the session's last turn")
		}
		at = *p.AtSequence
	}

	title := source.Title + " (fork)"
	if p.Title != nil {
		title, err = normalizeTitle(*p.Title)
		if err != nil {
			return Session{}, err
		}
	}

	now := s.now()
	id := NewID(now)

	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		rawMetadata, err := json.Marshal(source.Metadata)
		if err != nil {
			return err
		}
		rawPermissions, err := json.Marshal(source.Permissions)
		if err != nil {
			return err
		}

		const insertSession = `
    INSERT INTO vibe_sessions
        (id, parent_id, root_id, depth, title, agent_id, agent_slug, model,
         workspace, project, metadata, permissions, status, next_sequence, turn_count,
         forked_from_id, forked_from_sequence, last_activity_at, created_at, updated_at)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'open', 0, 0,
            $13, $14, $15, $15, $15)`

		if _, err := tx.Exec(ctx, insertSession, id, source.ParentID, source.RootID,
			source.Depth, title, source.AgentID, source.AgentSlug, source.Model,
			source.Workspace, source.Project, rawMetadata, rawPermissions, source.ID, at, now); err != nil {
			return err
		}

		if at > 0 {
			const selectTurns = `
    SELECT sequence, role, status, parts, body, model, agent_id,
           prompt_tokens, completion_tokens, cost_micros, created_at, completed_at
      FROM vibe_turns
     WHERE session_id = $1 AND sequence <= $2
     ORDER BY sequence`

			rows, err := tx.Query(ctx, selectTurns, source.ID, at)
			if err != nil {
				return err
			}

			type copiedTurn struct {
				sequence                 int64
				role                     string
				status                   string
				parts                    []byte
				body                     *string
				model                    *string
				agentID                  *ID
				prompt, completion, cost int64
				createdAt                time.Time
				completedAt              *time.Time
			}

			var copied []copiedTurn
			for rows.Next() {
				var t copiedTurn
				if err := rows.Scan(&t.sequence, &t.role, &t.status, &t.parts, &t.body, &t.model,
					&t.agentID, &t.prompt, &t.completion, &t.cost, &t.createdAt, &t.completedAt); err != nil {
					rows.Close()
					return err
				}
				copied = append(copied, t)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}

			const insertTurn = `
    INSERT INTO vibe_turns
        (id, session_id, sequence, role, status, parts, body, model, agent_id,
         prompt_tokens, completion_tokens, cost_micros, created_at, completed_at)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

			for _, t := range copied {
				if _, err := tx.Exec(ctx, insertTurn, NewID(now), id, t.sequence,
					t.role, t.status, t.parts, t.body, t.model, t.agentID,
					t.prompt, t.completion, t.cost, t.createdAt, t.completedAt); err != nil {
					return err
				}
			}
		}

		const finish = `
    UPDATE vibe_sessions
       SET next_sequence = coalesce((SELECT max(sequence) FROM vibe_turns WHERE session_id = $1), 0),
           turn_count = (SELECT count(*) FROM vibe_turns WHERE session_id = $1)
     WHERE id = $1`
		_, err = tx.Exec(ctx, finish, id)
		return err
	})
	if err != nil {
		return Session{}, unavailable(err)
	}

	return s.Session(ctx, id)
}

func (s *Service) appendSystemPart(ctx context.Context, tx pgx.Tx, sessionID ID, part Part, now time.Time) error {
	parts, _, err := NormalizeParts([]Part{part})
	if err != nil {
		return err
	}
	sequence, err := allocateSequence(ctx, tx, sessionID, now)
	if err != nil {
		return err
	}

	rawParts, err := json.Marshal(parts)
	if err != nil {
		return err
	}

	const insert = `
    INSERT INTO vibe_turns
        (id, session_id, sequence, role, status, parts, created_at, completed_at)
    VALUES ($1, $2, $3, 'system', 'complete', $4, $5, $5)`

	_, err = tx.Exec(ctx, insert, NewID(now), sessionID, sequence, rawParts, now)
	return err
}

func allocateSequence(ctx context.Context, tx pgx.Tx, sessionID ID, now time.Time) (int64, error) {
	const query = `
    UPDATE vibe_sessions
       SET next_sequence = next_sequence + 1,
           turn_count = turn_count + 1,
           last_activity_at = $2,
           updated_at = $2
     WHERE id = $1 AND status = 'open'
    RETURNING next_sequence`

	var sequence int64
	if err := tx.QueryRow(ctx, query, sessionID, now).Scan(&sequence); err != nil {
		if store.IsNoRows(err) {
			return 0, ErrClosed
		}
		return 0, err
	}
	return sequence, nil
}
