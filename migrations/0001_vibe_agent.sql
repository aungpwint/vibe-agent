-- vibe-agent's own schema. It is deliberately not part of the ThamBandha API
-- migrations: this database is the developer's local history for Vibe Coding
-- sessions, one database and one reader per machine.
--
-- A `vibe_` prefix keeps the tables recognisable inside a shared local Postgres
-- and makes a cross-wired DSN obvious at a glance. The tool additionally refuses
-- to run against the product databases (see config.Load), so the prefix is belt
-- and braces rather than the only guard.
--
-- Deliberate simplifications against the multi-reader design this is derived
-- from: no participants table (there is one developer), no per-reader read
-- markers and no change log (both are sync protocol for browsers nobody else
-- reads). What remains is what a local tool actually leans on: monotonic
-- per-session sequence allocation, client-id idempotency, one live run per
-- session, a depth cap on the session tree and trigram search over the
-- transcript.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE vibe_profiles (
    id            CHAR(26)    PRIMARY KEY,
    slug          TEXT        NOT NULL UNIQUE,
    name          TEXT        NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',

    mode          TEXT        NOT NULL DEFAULT 'all',
    builtin       BOOLEAN     NOT NULL DEFAULT false,

    system_prompt TEXT        NULL,
    model         TEXT        NULL,
    temperature   REAL        NULL,

    tools         JSONB       NOT NULL DEFAULT '[]'::jsonb,
    permissions   JSONB       NOT NULL DEFAULT '[]'::jsonb,
    metadata      JSONB       NOT NULL DEFAULT '{}'::jsonb,

    archived_at   TIMESTAMPTZ NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT vibe_profiles_mode_known
        CHECK (mode IN ('primary', 'subagent', 'all')),
    CONSTRAINT vibe_profiles_slug_shape
        CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    CONSTRAINT vibe_profiles_name_length
        CHECK (char_length(name) BETWEEN 1 AND 120),
    CONSTRAINT vibe_profiles_description_length
        CHECK (char_length(description) <= 2000),
    CONSTRAINT vibe_profiles_temperature_range
        CHECK (temperature IS NULL OR (temperature >= 0 AND temperature <= 2)),
    CONSTRAINT vibe_profiles_tools_array
        CHECK (jsonb_typeof(tools) = 'array'),
    CONSTRAINT vibe_profiles_permissions_array
        CHECK (jsonb_typeof(permissions) = 'array'),
    CONSTRAINT vibe_profiles_metadata_object
        CHECK (jsonb_typeof(metadata) = 'object')
);

-- The built-in definitions, with fixed ids so an export from one machine
-- imports into another without resolving profiles by name first. Slugs are the
-- names client tools already use for primary and subagent work.
INSERT INTO vibe_profiles (id, slug, name, description, mode, builtin, tools, permissions)
VALUES
    ('00000000000000000000000001', 'build', 'Build',
     'The default primary agent: writes code, runs commands and finishes the task.',
     'primary', true,
     '["read", "write", "edit", "bash", "grep", "glob", "webfetch", "task"]'::jsonb,
     '[{"action": "edit", "resource": "*", "effect": "allow"},
       {"action": "bash", "resource": "*", "effect": "ask"},
       {"action": "webfetch", "resource": "*", "effect": "allow"},
       {"action": "task", "resource": "*", "effect": "ask"}]'::jsonb),
    ('00000000000000000000000002', 'plan', 'Plan',
     'A read-only primary agent: investigates and proposes, never edits.',
     'primary', true,
     '["read", "grep", "glob", "webfetch", "task"]'::jsonb,
     '[{"action": "edit", "resource": "*", "effect": "deny"},
       {"action": "bash", "resource": "*", "effect": "ask"},
       {"action": "webfetch", "resource": "*", "effect": "allow"},
       {"action": "task", "resource": "*", "effect": "ask"}]'::jsonb),
    ('00000000000000000000000003', 'general', 'General',
     'A subagent for self-contained work a primary agent delegates.',
     'subagent', true,
     '["read", "write", "edit", "bash", "grep", "glob", "webfetch", "task"]'::jsonb,
     '[{"action": "edit", "resource": "*", "effect": "allow"},
       {"action": "bash", "resource": "*", "effect": "ask"},
       {"action": "webfetch", "resource": "*", "effect": "allow"},
       {"action": "task", "resource": "*", "effect": "allow"}]'::jsonb),
    ('00000000000000000000000004', 'explore', 'Explore',
     'A read-only subagent for searching a codebase and reporting what it found.',
     'subagent', true,
     '["read", "grep", "glob"]'::jsonb,
     '[{"action": "edit", "resource": "*", "effect": "deny"},
       {"action": "bash", "resource": "*", "effect": "deny"},
       {"action": "webfetch", "resource": "*", "effect": "deny"},
       {"action": "task", "resource": "*", "effect": "deny"}]'::jsonb)
ON CONFLICT (slug) DO NOTHING;

CREATE TABLE vibe_sessions (
    id                   CHAR(26)    PRIMARY KEY,

    parent_id            CHAR(26)    NULL REFERENCES vibe_sessions (id) ON DELETE CASCADE,
    root_id              CHAR(26)    NULL REFERENCES vibe_sessions (id) ON DELETE CASCADE,
    depth                SMALLINT    NOT NULL DEFAULT 0,

    title                TEXT        NOT NULL,
    agent_id             CHAR(26)    NOT NULL REFERENCES vibe_profiles (id) ON DELETE RESTRICT,
    agent_slug           TEXT        NOT NULL,
    model                TEXT        NULL,
    workspace            TEXT        NULL,
    project              TEXT        NULL,

    metadata             JSONB       NOT NULL DEFAULT '{}'::jsonb,
    permissions          JSONB       NOT NULL DEFAULT '[]'::jsonb,

    status               TEXT        NOT NULL DEFAULT 'open',

    next_sequence        BIGINT      NOT NULL DEFAULT 0,
    turn_count           INTEGER     NOT NULL DEFAULT 0,
    prompt_tokens        BIGINT      NOT NULL DEFAULT 0,
    completion_tokens    BIGINT      NOT NULL DEFAULT 0,
    cost_micros          BIGINT      NOT NULL DEFAULT 0,

    forked_from_id       CHAR(26)    NULL REFERENCES vibe_sessions (id) ON DELETE SET NULL,
    forked_from_sequence BIGINT      NULL,

    archived_at          TIMESTAMPTZ NULL,
    last_activity_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at            TIMESTAMPTZ NULL,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT vibe_sessions_status_known
        CHECK (status IN ('open', 'closed')),
    CONSTRAINT vibe_sessions_title_length
        CHECK (char_length(title) BETWEEN 1 AND 200),
    CONSTRAINT vibe_sessions_depth_range
        CHECK (depth >= 0 AND depth <= 32),
    CONSTRAINT vibe_sessions_metadata_object
        CHECK (jsonb_typeof(metadata) = 'object'),
    CONSTRAINT vibe_sessions_permissions_array
        CHECK (jsonb_typeof(permissions) = 'array'),
    CONSTRAINT vibe_sessions_parent_not_self
        CHECK (parent_id IS NULL OR parent_id <> id),
    CONSTRAINT vibe_sessions_root_matches_parent
        CHECK ((parent_id IS NULL) = (root_id IS NULL)),
    CONSTRAINT vibe_sessions_fork_pair
        CHECK ((forked_from_id IS NULL) = (forked_from_sequence IS NULL)),
    CONSTRAINT vibe_sessions_tokens_non_negative
        CHECK (prompt_tokens >= 0 AND completion_tokens >= 0 AND cost_micros >= 0)
);

-- The session list, newest activity first. Archived sessions fall out of the
-- default ordering index entirely.
CREATE INDEX vibe_sessions_activity_idx
    ON vibe_sessions (last_activity_at DESC, id DESC)
    WHERE archived_at IS NULL;

-- Children of a session, and every session under one root.
CREATE INDEX vibe_sessions_parent_idx
    ON vibe_sessions (parent_id, created_at, id)
    WHERE parent_id IS NOT NULL;
CREATE INDEX vibe_sessions_root_idx
    ON vibe_sessions (root_id, created_at, id)
    WHERE root_id IS NOT NULL;

CREATE TABLE vibe_runs (
    id               CHAR(26)    PRIMARY KEY,
    session_id       CHAR(26)    NOT NULL REFERENCES vibe_sessions (id) ON DELETE CASCADE,
    agent_id         CHAR(26)    NULL REFERENCES vibe_profiles (id) ON DELETE SET NULL,

    status           TEXT        NOT NULL DEFAULT 'queued',
    model            TEXT        NULL,

    child_session_id CHAR(26)    NULL REFERENCES vibe_sessions (id) ON DELETE SET NULL,

    steps            INTEGER     NOT NULL DEFAULT 0,
    prompt_tokens    INTEGER     NOT NULL DEFAULT 0,
    completion_tokens INTEGER    NOT NULL DEFAULT 0,
    cost_micros      BIGINT      NOT NULL DEFAULT 0,

    error            TEXT        NULL,

    started_at       TIMESTAMPTZ NULL,
    finished_at      TIMESTAMPTZ NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT vibe_runs_status_known
        CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled')),
    CONSTRAINT vibe_runs_steps_non_negative
        CHECK (steps >= 0 AND prompt_tokens >= 0 AND completion_tokens >= 0 AND cost_micros >= 0),
    CONSTRAINT vibe_runs_error_length
        CHECK (error IS NULL OR char_length(error) <= 8000),
    CONSTRAINT vibe_runs_finished_status
        CHECK ((status IN ('completed', 'failed', 'cancelled')) = (finished_at IS NOT NULL))
);

-- At most one queued or running run per session. A second start is a conflict
-- rather than two drains stepping over each other; the partial index is the
-- arbiter under concurrency.
CREATE UNIQUE INDEX vibe_runs_live_key
    ON vibe_runs (session_id)
    WHERE status IN ('queued', 'running');

CREATE INDEX vibe_runs_session_idx ON vibe_runs (session_id, created_at DESC, id DESC);
CREATE INDEX vibe_runs_status_idx ON vibe_runs (status, created_at);

CREATE TABLE vibe_turns (
    id                CHAR(26)    PRIMARY KEY,
    session_id        CHAR(26)    NOT NULL REFERENCES vibe_sessions (id) ON DELETE CASCADE,
    sequence          BIGINT      NOT NULL,

    role              TEXT        NOT NULL,
    status            TEXT        NOT NULL DEFAULT 'complete',

    parent_turn_id    CHAR(26)    NULL REFERENCES vibe_turns (id) ON DELETE SET NULL,

    parts             JSONB       NOT NULL DEFAULT '[]'::jsonb,
    body              TEXT        NULL,

    model             TEXT        NULL,
    agent_id          CHAR(26)    NULL REFERENCES vibe_profiles (id) ON DELETE SET NULL,
    run_id            CHAR(26)    NULL REFERENCES vibe_runs (id) ON DELETE SET NULL,

    prompt_tokens     INTEGER     NOT NULL DEFAULT 0,
    completion_tokens INTEGER     NOT NULL DEFAULT 0,
    cost_micros       BIGINT      NOT NULL DEFAULT 0,

    client_id         TEXT        NULL,

    redacted_at       TIMESTAMPTZ NULL,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ NULL,

    CONSTRAINT vibe_turns_sequence_positive
        CHECK (sequence > 0),
    CONSTRAINT vibe_turns_role_known
        CHECK (role IN ('user', 'assistant', 'system', 'tool')),
    CONSTRAINT vibe_turns_status_known
        CHECK (status IN ('streaming', 'complete', 'error', 'aborted')),
    CONSTRAINT vibe_turns_parts_array
        CHECK (jsonb_typeof(parts) = 'array'),
    CONSTRAINT vibe_turns_body_length
        CHECK (body IS NULL OR char_length(body) <= 32000),
    CONSTRAINT vibe_turns_tokens_non_negative
        CHECK (prompt_tokens >= 0 AND completion_tokens >= 0 AND cost_micros >= 0),
    CONSTRAINT vibe_turns_sequence_unique
        UNIQUE (session_id, sequence)
);

-- A retried append carries the same client id and dedupes instead of becoming a
-- second turn with a second sequence.
CREATE UNIQUE INDEX vibe_turns_client_id_key
    ON vibe_turns (session_id, client_id)
    WHERE client_id IS NOT NULL;

CREATE INDEX vibe_turns_session_idx ON vibe_turns (session_id, sequence);

-- The indexed expression is the one the search matches on, so the planner can
-- use it.
CREATE INDEX vibe_turns_search_idx
    ON vibe_turns USING GIN (lower(coalesce(body, '')) gin_trgm_ops);
