# vibe-agent

Local-only session history for Vibe Coding: agent profiles, a session tree,
typed turns and runs, kept in the developer's own PostgreSQL database.

`vibe-agent` is a standalone Go module. It links no application code, starts no
server and does not use Redis. The history of every project you work on lives in
one local database, and the tool reads exactly one environment variable:
`VIBE_AGENT_DATABASE_URL`.

## Why it is safe to run next to production credentials

The tool is local-only by construction, not by convention:

| Guard | Behaviour |
|---|---|
| Own variable | It reads exactly `VIBE_AGENT_DATABASE_URL` and never falls back to `DATABASE_URL` or any other application variable. |
| Loopback only | The DSN must point at `localhost`, `127.0.0.1`, `::1` or a unix socket. A remote host is refused before connecting. |
| Application databases refused | A built-in denylist in `config` refuses known application database names, so a mis-pasted DSN cannot be used. |
| No server, no Redis | Every command is a short-lived process; nothing listens, and Redis is not a dependency. |
| Own schema | Tables are `vibe_`-prefixed and live in their own database, not in an application schema. |

The guard is not a security boundary against a hostile local user - it exists so
that a copy of this tool on a machine with production credentials cannot point
itself at production by accident.

## Requirements

- Go 1.27+ to build.
- A local PostgreSQL 14+ server. The schema uses `pg_trgm`, `jsonb` and partial
  unique indexes. The connecting role needs permission to create a database
  (for `db create`) and to install the extension, unless both already exist.

## Quick start

Set the DSN once per shell. The value names the database the tool will create
and use; it must be on this machine.

```powershell
# PowerShell
$env:VIBE_AGENT_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/vibe_agent?sslmode=disable'
```

```sh
# POSIX shells
export VIBE_AGENT_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/vibe_agent?sslmode=disable'
```

Build the binary and prepare the database:

```powershell
go build -o vibe-agent.exe ./cmd/vibe-agent
.\vibe-agent.exe db create     # creates the database if it is missing
.\vibe-agent.exe db migrate    # applies the embedded schema
.\vibe-agent.exe doctor        # config, connection, schema and profile counts
```

On macOS and Linux, build with `go build -o vibe-agent ./cmd/vibe-agent` and run
`./vibe-agent`.

Then record a session:

```powershell
$session = .\vibe-agent.exe session new --title "Refactor parser" --agent build `
    --project billing-api --workspace C:\work\billing-api --json | ConvertFrom-Json
$sid = $session.id

.\vibe-agent.exe turn append --session $sid --text "Split the lexer out of the parser"
.\vibe-agent.exe turn append --session $sid --role assistant --status streaming --text "Working on it"
.\vibe-agent.exe turn list --session $sid

$run = .\vibe-agent.exe run start --session $sid --json | ConvertFrom-Json
.\vibe-agent.exe run finish $run.id --status completed --steps 3 `
    --tokens-prompt 1200 --tokens-completion 340 --cost-micros 4200

.\vibe-agent.exe session export $sid --out .\parser-session.json
.\vibe-agent.exe search "lexer"
```

Every command accepts `--json` where a structured answer is useful, so the tool
can be driven from scripts.

## One database, every project

The database is per machine, not per repository, so sessions from every project
share one history. Tag each session when you start it:

- `--project NAME` - a short, free-form project label (`billing-api`).
- `--workspace PATH` - the directory the session works in.

Both are optional and are shown by `session show`; `session fork` and
`session export`/`import` carry them along. `session list --json` and
`session show --json` return `project` and `workspace`, so a script can select
one project:

```powershell
.\vibe-agent.exe session list --all --limit 100 --json |
    ConvertFrom-Json | Where-Object { $_.project -eq 'billing-api' }
```

Four profiles (`build`, `plan`, `general`, `explore`) are available in every
project. Create custom profiles when a project or workflow needs its own
tooling or permission rules.

## Commands

```
vibe-agent db create|migrate|status
vibe-agent doctor
vibe-agent profile list|show|create|update|archive|restore
vibe-agent session new|list|show|close|reopen|archive|unarchive|rm|fork|export|import
vibe-agent turn append|list|complete|redact
vibe-agent run start|finish|cancel|list
vibe-agent search TEXT
vibe-agent help | version
```

Run `vibe-agent help` for the flag-level reference.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | unexpected failure |
| 2 | usage, validation or configuration error |
| 3 | not found |
| 4 | conflict (closed session, duplicate slug, second live run, state transition) |
| 5 | the local database is unavailable |

## What each noun means

- **Profile** - an agent definition: slug, mode (`primary`, `subagent`, `all`),
  optional model/prompt/temperature, tool names and a permission ruleset. Four
  built-in profiles (`build`, `plan`, `general`, `explore`) ship with the
  schema and cannot be edited or archived. Custom profiles can be archived,
  which only affects new sessions.
- **Session** - one conversation. Sessions form a tree: a `primary` agent
  creates a child session only through a `subagent` profile, up to depth 32.
  A session is `open` or `closed`; archiving is a filing decision that does not
  block turns. `fork` copies the transcript up to a sequence into a new
  session. A session may also carry an optional `project` and `workspace` tag.
- **Turn** - one message with a typed part list (`text`, `reasoning`, `tool`,
  `file`, `snapshot`, `patch`, `step`, `agent`, `model`). Turns carry a
  per-session sequence, optional token/cost accounting and an optional
  `client_id` for idempotent retries.
- **Run** - one execution of an agent in a session: `queued`, `running` and a
  terminal state, with step and token totals. At most one run per session may
  be live at a time.

## Concurrency and data integrity

These are enforced by the schema and the service, and covered by the live tests:

- Per-session sequences are handed out under a row lock, so concurrent appends
  receive `1..N` without gaps or duplicates; `(session_id, sequence)` is unique.
- A retried append with the same `--client-id` returns the original turn instead
  of allocating a second sequence. A partial unique index is the arbiter under
  a race, and the losing transaction rolls its sequence back.
- Session token totals only move forward; completing a turn applies the delta
  inside the same transaction, and a smaller total is rejected.
- A partial unique index allows one `queued`/`running` run per session.
- Redaction clears a turn's parts and body and keeps its sequence, so the
  transcript stays readable; it cannot be undone.
- `fork` and `import` copy transcripts in one transaction.

## Tests

```powershell
# unit tests only (no database needed)
go test ./...

# create the throwaway database once (db create reads VIBE_AGENT_DATABASE_URL)
$env:VIBE_AGENT_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/vibe_agent_test?sslmode=disable'
.\vibe-agent.exe db create

# TestMain applies the schema and resets sessions/custom profiles per test
$env:VIBE_AGENT_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/vibe_agent_test?sslmode=disable'
go test ./... -count=1
```

The live tests cover sequencing, idempotency, concurrent appends, token
monotonicity, the one-live-run rule, fork/export/import, search, redaction,
the session tree cap and the profile lifecycle. They skip themselves when
`VIBE_AGENT_TEST_DATABASE_URL` is unset, so `go test ./...` is safe without a
database.

## Layout

```
cmd/vibe-agent/     CLI: argument handling, rendering, exit codes
agent/              domain service: profiles, sessions, turns, runs, export
store/              pgx pool, migration runner, database creation
config/             the local-only DSN guard
migrations/         embedded SQL schema (immutable once shipped)
docs/               design notes
```

## Documentation

- `docs/DESIGN.md` - architecture, data model, invariants and design decisions.
