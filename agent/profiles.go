package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/aungpwint/vibe-agent/store"
)

const profileColumns = `
    id, slug, name, description, mode, builtin, system_prompt, model, temperature,
    tools, permissions, metadata, archived_at, created_at, updated_at`

func scanProfile(row rowScanner) (Profile, error) {
	var (
		p           Profile
		mode        string
		tools       []byte
		permissions []byte
		metadata    []byte
	)
	if err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &mode, &p.Builtin,
		&p.SystemPrompt, &p.Model, &p.Temperature, &tools, &permissions, &metadata,
		&p.ArchivedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Profile{}, err
	}

	p.Mode = AgentMode(mode)

	var err error
	if p.Tools, err = decodeTools(tools); err != nil {
		return Profile{}, err
	}
	if p.Permissions, err = decodeRuleset(permissions); err != nil {
		return Profile{}, err
	}
	if p.Metadata, err = decodeMetadata(metadata); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// Profiles lists the active agent definitions, built-ins first.
func (s *Service) Profiles(ctx context.Context, includeArchived bool) ([]Profile, error) {
	const query = `
    SELECT ` + profileColumns + `
      FROM vibe_profiles
     WHERE archived_at IS NULL OR $1
     ORDER BY builtin DESC, name, id`

	rows, err := s.pool.Query(ctx, query, includeArchived)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()

	out := make([]Profile, 0, 8)
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

func (s *Service) Profile(ctx context.Context, id ID) (Profile, error) {
	const query = `SELECT ` + profileColumns + ` FROM vibe_profiles WHERE id = $1`

	p, err := scanProfile(s.pool.QueryRow(ctx, query, id))
	if err != nil {
		return Profile{}, profileError(err)
	}
	return p, nil
}

// ProfileBySlug is the lookup every session create and import goes through, so
// a miss is reported as a validation problem by its callers.
func (s *Service) ProfileBySlug(ctx context.Context, slug string) (Profile, error) {
	normalized, err := normalizeSlug(slug)
	if err != nil {
		return Profile{}, err
	}

	const query = `SELECT ` + profileColumns + ` FROM vibe_profiles WHERE slug = $1`

	p, err := scanProfile(s.pool.QueryRow(ctx, query, normalized))
	if err != nil {
		return Profile{}, profileError(err)
	}
	return p, nil
}

// ResolveProfile accepts an id or a slug, which is what the CLI wants.
func (s *Service) ResolveProfile(ctx context.Context, ref string) (Profile, error) {
	ref = strings.TrimSpace(ref)
	if id, err := ParseID(ref); err == nil {
		return s.Profile(ctx, id)
	}
	return s.ProfileBySlug(ctx, ref)
}

func (s *Service) CreateProfile(ctx context.Context, p CreateProfileParams) (Profile, error) {
	slug, err := normalizeSlug(p.Slug)
	if err != nil {
		return Profile{}, err
	}
	name := strings.TrimSpace(p.Name)
	if name == "" || len([]rune(name)) > 120 {
		return Profile{}, invalid("an agent needs a name of 1 to 120 characters")
	}
	if len([]rune(p.Description)) > maxDescriptionRunes {
		return Profile{}, invalid("that description is too long")
	}
	if p.Mode == "" {
		p.Mode = ModeAll
	}
	if !p.Mode.valid() {
		return Profile{}, invalid("an agent's mode must be primary, subagent or all")
	}
	if err := validateTemperature(p.Temperature); err != nil {
		return Profile{}, err
	}
	permissions, err := NormalizeRuleset(p.Permissions)
	if err != nil {
		return Profile{}, err
	}
	metadata, err := normalizeMetadata(p.Metadata)
	if err != nil {
		return Profile{}, err
	}

	now := s.now()
	id := NewID(now)
	tools := dedupeStrings(p.Tools)

	rawTools, err := json.Marshal(tools)
	if err != nil {
		return Profile{}, unavailable(err)
	}
	rawPermissions, err := json.Marshal(permissions)
	if err != nil {
		return Profile{}, unavailable(err)
	}
	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		return Profile{}, unavailable(err)
	}

	const insert = `
    INSERT INTO vibe_profiles
        (id, slug, name, description, mode, builtin, system_prompt, model, temperature,
         tools, permissions, metadata, created_at, updated_at)
    VALUES ($1, $2, $3, $4, $5, false, $6, $7, $8, $9, $10, $11, $12, $12)
    RETURNING ` + profileColumns

	created, err := scanProfile(s.pool.QueryRow(ctx, insert,
		id, slug, name, strings.TrimSpace(p.Description), string(p.Mode), p.SystemPrompt,
		p.Model, p.Temperature, rawTools, rawPermissions, rawMetadata, now))
	if err != nil {
		switch {
		case store.IsUniqueViolation(err):
			return Profile{}, conflict("an agent with the slug %q already exists", slug)
		case store.IsCheckViolation(err), store.IsForeignKeyViolation(err):
			return Profile{}, invalid("that agent cannot be created as it is")
		default:
			return Profile{}, unavailable(err)
		}
	}
	return created, nil
}

func (s *Service) UpdateProfile(ctx context.Context, p UpdateProfileParams) (Profile, error) {
	var updated Profile

	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		const query = `SELECT ` + profileColumns + ` FROM vibe_profiles WHERE id = $1 FOR UPDATE`

		current, err := scanProfile(tx.QueryRow(ctx, query, p.ID))
		if err != nil {
			return profileError(err)
		}
		if current.Builtin {
			return conflict("the built-in agents cannot be edited")
		}
		if current.ArchivedAt != nil {
			return conflict("that agent is archived; restore it before editing")
		}

		if p.Name != nil {
			current.Name = strings.TrimSpace(*p.Name)
			if current.Name == "" || len([]rune(current.Name)) > 120 {
				return invalid("an agent needs a name of 1 to 120 characters")
			}
		}
		if p.Description != nil {
			if len([]rune(*p.Description)) > maxDescriptionRunes {
				return invalid("that description is too long")
			}
			current.Description = strings.TrimSpace(*p.Description)
		}
		if p.Mode != nil {
			if !p.Mode.valid() {
				return invalid("an agent's mode must be primary, subagent or all")
			}
			current.Mode = *p.Mode
		}
		if p.SystemPrompt != nil {
			current.SystemPrompt, err = optionalText(p.SystemPrompt, 0, "system prompt")
			if err != nil {
				return err
			}
		}
		if p.Model != nil {
			current.Model, err = optionalText(p.Model, maxModelLength, "model")
			if err != nil {
				return err
			}
		}
		if p.Temperature != nil {
			if err := validateTemperature(p.Temperature); err != nil {
				return err
			}
			current.Temperature = p.Temperature
		}
		if p.Tools != nil {
			current.Tools = dedupeStrings(*p.Tools)
		}
		if p.Permissions != nil {
			current.Permissions, err = NormalizeRuleset(*p.Permissions)
			if err != nil {
				return err
			}
		}
		if p.Metadata != nil {
			current.Metadata, err = normalizeMetadata(*p.Metadata)
			if err != nil {
				return err
			}
		}

		rawTools, err := json.Marshal(current.Tools)
		if err != nil {
			return err
		}
		rawPermissions, err := json.Marshal(current.Permissions)
		if err != nil {
			return err
		}
		rawMetadata, err := json.Marshal(current.Metadata)
		if err != nil {
			return err
		}

		const update = `
    UPDATE vibe_profiles
       SET name = $2, description = $3, mode = $4, system_prompt = $5, model = $6,
           temperature = $7, tools = $8, permissions = $9, metadata = $10, updated_at = now()
     WHERE id = $1
    RETURNING ` + profileColumns

		updated, err = scanProfile(tx.QueryRow(ctx, update, p.ID,
			current.Name, current.Description, string(current.Mode), current.SystemPrompt,
			current.Model, current.Temperature, rawTools, rawPermissions, rawMetadata))
		return err
	})
	if err != nil {
		return Profile{}, err
	}
	return updated, nil
}

// ArchiveProfile retires a custom agent without deleting it: sessions that
// already reference it keep resolving, while new sessions cannot choose it.
func (s *Service) ArchiveProfile(ctx context.Context, ref string) (Profile, error) {
	current, err := s.ResolveProfile(ctx, ref)
	if err != nil {
		return Profile{}, err
	}
	if current.Builtin {
		return Profile{}, conflict("the built-in agents cannot be archived")
	}
	if current.ArchivedAt != nil {
		return current, nil
	}

	const update = `
    UPDATE vibe_profiles SET archived_at = now(), updated_at = now()
     WHERE id = $1
    RETURNING ` + profileColumns

	archived, err := scanProfile(s.pool.QueryRow(ctx, update, current.ID))
	if err != nil {
		return Profile{}, unavailable(err)
	}
	return archived, nil
}

// RestoreProfile undoes ArchiveProfile.
func (s *Service) RestoreProfile(ctx context.Context, ref string) (Profile, error) {
	current, err := s.ResolveProfile(ctx, ref)
	if err != nil {
		return Profile{}, err
	}
	if current.ArchivedAt == nil {
		return current, nil
	}

	const update = `
    UPDATE vibe_profiles SET archived_at = NULL, updated_at = now()
     WHERE id = $1
    RETURNING ` + profileColumns

	restored, err := scanProfile(s.pool.QueryRow(ctx, update, current.ID))
	if err != nil {
		return Profile{}, unavailable(err)
	}
	return restored, nil
}

// resolveAgent is the shared "which agent is this session for" step: it maps a
// slug to a live profile and refuses archived definitions.
func (s *Service) resolveAgent(ctx context.Context, slug string) (Profile, error) {
	if strings.TrimSpace(slug) == "" {
		slug = "build"
	}
	profile, err := s.ProfileBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, ErrProfileNotFound) {
			return Profile{}, invalid("there is no agent named %q in the local store", slug)
		}
		return Profile{}, err
	}
	if profile.ArchivedAt != nil {
		return Profile{}, invalid("the agent %q is archived", slug)
	}
	return profile, nil
}
