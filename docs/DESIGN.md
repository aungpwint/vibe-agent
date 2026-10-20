# Design

## Goal

Keep a durable, queryable history of local Vibe Coding sessions - what an agent
was asked, what it answered, which tools ran, what it cost - in a database that
belongs to the developer and to no application.

## Non-goals

- Serving a browser or a network peer. The tool is a CLI; there is no HTTP
  listener, no WebSocket, no GraphQL and no realtime fan-out.
- Multi-user or multi-reader sync. There is exactly one developer per machine,
  one database and one reader at a time.
- Orchestrating agents. It records sessions and runs; it does not start them.
- Running against a shared or remote database. See the guard section.

## Architecture

```
cmd/vibe-agent          CLI: flags, rendering, exit codes, --json
        |
        v
agent.Service           domain rules and transactions
        |
        v
store.Pool              pgx pool, InTx helper, migration runner, db creation
        |
        v
local PostgreSQL        database `vibe_agent`, tables `vibe_*`
```

- `config` sits beside the CLI and decides which DSN is acceptable before any
  connection exists.
- `migrations` embeds the SQL files; the runner applies them in order, records
  a ledger row per file (version, name, checksum, timestamp) and takes a
  session-level advisory lock so two commands cannot migrate at once.
- The CLI never reaches around `agent.Service` for writes, so every rule -
  sequence allocation, token totals, transitions, idempotency - has one
  implementation.

## Data model

### `vibe_profiles`

Agent definitions. Fixed ULID-shaped ids let an export from one machine import
into another regardless of profile creation order. `builtin` profiles are
protected in the service layer (no update, no archive). `mode` gates the
session tree: a `primary` session may only create children through a
`subagent` profile. `tools` and `permissions` are `jsonb` arrays; a permission
rule is `{action, resource, effect}` and the last matching rule wins, with
default `ask`.

### `vibe_sessions`

One conversation. `parent_id`/`root_id`/`depth` form the tree (depth 0-32);
`agent_slug` is denormalised for cheap filtering while `agent_id` keeps the
foreign key. `project` and `workspace` are optional free-form tags: they let one
database hold the history of several projects, and they are copied by a fork and
by export/import. Counters (`next_sequence`, `turn_count`, `prompt_tokens`,
`completion_tokens`, `cost_micros`) are maintained transactionally and are the
session's authoritative totals. `forked_from_id`/`forked_from_sequence` record
lineage; `archived_at` is a filing flag, `status` is the open/closed lifecycle.

### `vibe_turns`

One message. `parts` is the typed content (`text`, `reasoning`, `tool`, `file`,
`snapshot`, `patch`, `step`, `agent`, `model`); `body` is the derived
searchable text of the `text` parts only, so reasoning is not searchable by
accident. `sequence` is unique per session. `client_id` makes retried appends
idempotent. `redacted_at` marks a scrubbed turn: parts and body are cleared,
the sequence and token totals stay.

### `vibe_runs`

One execution of an agent in a session. `queued`/`running` are live; the
partial unique index `vibe_runs_live_key` allows one live run per session.
`steps`, token fields and `child_session_id` summarise the work; a check
constraint ties the three terminal statuses to `finished_at`.

## Invariants and how they are kept

| Invariant | Mechanism |
|---|---|
| Per-session sequences are dense and unique | `UPDATE vibe_sessions ... RETURNING next_sequence` under the row lock, then insert; `UNIQUE (session_id, sequence)` |
| A retried append does not duplicate | pre-check on `client_id`, then the partial unique index; a losing insert reads the winner back and compensates the counter in the same transaction |
| Token totals move forward only | delta computed from the stored turn, applied inside the completion transaction; negative deltas rejected |
| One live run per session | partial unique index plus a service-level conflict |
| Fork is atomic | one transaction inserts the copy, its turns bounded by `at sequence`, then sets counters |
| Tree depth cannot exceed 32 | depth computed from the parent under `SELECT ... FOR UPDATE`; the schema also refuses `depth > 32` |
| Turn content changes only while streaming | completion requires `status = 'streaming'`; a second completion with the same status is a read, a different status is a conflict |
| Redaction is final | the update is unconditional and clears parts/body; the service never restores them |

## Local-only guard

`config.Load` reads exactly `VIBE_AGENT_DATABASE_URL`. The DSN must resolve to
a loopback host (or a unix socket), and the database name must not be on the
built-in denylist (`config/productDatabases`; currently `thambandha`,
`thambandha_api`, `thambandha_test`). There is no environment variable, flag or
config file that relaxes either rule, and the tests pin that behaviour. The
worst case on a machine with production credentials is a refused command, not a
production connection.

## Scope

The tool serves one developer on one machine. That keeps the surface small:

- It records profiles, the session tree, turns, runs, idempotency, per-session
  sequencing, fork, search, export/import and token/cost accounting.
- It has no accounts, participants, roles, ownership, read markers, change log,
  pinned/unread flags, realtime events, GraphQL or HTTP surface. A single local
  reader needs none of them, and each would be another invariant to test.
- It uses one embedded schema instead of an application's migration chain, so
  the two databases never share a ledger.
- Every command is a short-lived process; there is no daemon.

## Migration policy

`migrations/0001_vibe_agent.sql` is immutable once shipped - it carries the
schema, constraints, indexes and the four built-in profiles. Changes to a
shipped schema are new numbered files. The runner:

1. reads the embedded directory and sorts by version;
2. compares checksums with the ledger and fails on drift;
3. applies pending files in order inside a transaction each;
4. records `(version, name, checksum)` in `vibe_schema_migrations`;
5. takes `pg_advisory_xact_lock` so concurrent commands serialise.

`db status` lists applied and pending files; `doctor` adds a connection check
and a profile census.

## Testing

- Unit tests (no database): ruleset resolution, part normalisation, ULID
  monotonicity, enum parsing, the config guard and the migration helpers.
- Live tests (`VIBE_AGENT_TEST_DATABASE_URL`): the full service surface -
  lifecycle, tree cap, sequencing and idempotency, eight concurrent appends
  landing on `1..8`, token deltas, the live-run rule, fork, export/import,
  search (including redaction exclusion), profile lifecycle and model-change
  system turns.

The live suite deletes sessions and custom profiles between tests and leaves
the four built-in profiles in place.
