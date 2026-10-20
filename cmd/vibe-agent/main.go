package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aungpwint/vibe-agent/agent"
	"github.com/aungpwint/vibe-agent/config"
	"github.com/aungpwint/vibe-agent/store"
)

const version = "0.1.0"

var errUsage = errors.New("usage")

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func (e *usageError) Is(target error) bool { return target == errUsage }

func usagef(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "vibe-agent: %v\n", err)
		os.Exit(exitCode(err))
	}
}

func exitCode(err error) int {
	switch {
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, config.ErrConfig):
		return 2
	case errors.Is(err, agent.ErrInvalidID):
		return 2
	}
	switch agent.CodeOf(err) {
	case agent.CodeNotFound:
		return 3
	case agent.CodeValidation:
		return 2
	case agent.CodeConflict:
		return 4
	case agent.CodeUnavailable:
		return 5
	default:
		return 1
	}
}

func run(args []string, w io.Writer) error {
	if len(args) == 0 {
		printUsage(w)
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		printUsage(w)
		return nil
	case "version", "-v", "--version":
		fmt.Fprintf(w, "vibe-agent %s\n", version)
		return nil
	case "db":
		return runDB(args[1:], w)
	case "doctor":
		return runDoctor(w)
	case "profile", "profiles":
		return runProfile(args[1:], w)
	case "session", "sessions":
		return runSession(args[1:], w)
	case "turn", "turns":
		return runTurn(args[1:], w)
	case "run", "runs":
		return runRun(args[1:], w)
	case "search":
		return runSearch(args[1:], w)
	default:
		return usagef("unknown command %q (run `vibe-agent help`)", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `vibe-agent - local Vibe Coding session history

The tool reads VIBE_AGENT_DATABASE_URL, requires a loopback PostgreSQL server and
refuses the ThamBandha database names. It never starts a server and never talks
to Redis.

  vibe-agent db create                 create the local database if missing
  vibe-agent db migrate                apply the embedded schema
  vibe-agent db status                 show applied and pending migrations
  vibe-agent doctor                    check config, connection and schema

  vibe-agent profile list [--all] [--json]
  vibe-agent profile show <id|slug> [--json]
  vibe-agent profile create --slug S --name N [--mode primary|subagent|all]
        [--description D] [--model M] [--prompt P] [--temperature T]
        [--tools a,b] [--permission allow:edit=*]... [--permissions '<json>']
        [--meta k=v]...
  vibe-agent profile update <id|slug> [--name N] [--description D] [--mode M]
        [--model M] [--prompt P] [--temperature T] [--tools a,b]
        [--permission allow:edit=*]... [--permissions '<json>'] [--meta k=v]...
  vibe-agent profile archive|restore <id|slug>

  vibe-agent session new --title T [--agent build] [--parent ID] [--model M]
        [--workspace W] [--project P] [--meta k=v]... [--json]
  vibe-agent session list [--limit N] [--agent SLUG] [--status open|closed]
        [--parent ID] [--root ID] [--archived|--all] [--json]
  vibe-agent session show <id> [--turns] [--limit N] [--json]
  vibe-agent session close|reopen|archive|unarchive|rm <id>
  vibe-agent session fork <id> [--at SEQUENCE] [--title T] [--json]
  vibe-agent session export <id> [--out FILE]
  vibe-agent session import FILE [--parent ID] [--json]

  vibe-agent turn append --session ID (--text TEXT | --part JSON | --parts-file F)
        [--role user|assistant|system|tool] [--status streaming|complete|error|aborted]
        [--client-id C] [--model M] [--run ID] [--parent ID]
        [--tokens-prompt N] [--tokens-completion N] [--cost-micros N] [--json]
  vibe-agent turn list --session ID [--after N] [--limit N] [--role R] [--json]
  vibe-agent turn complete <id> [--status complete|error|aborted]
        [--text TEXT | --part JSON | --parts-file F]
        [--tokens-prompt N] [--tokens-completion N] [--cost-micros N] [--json]
  vibe-agent turn redact <id> [--json]

  vibe-agent search TEXT [--session ID] [--role R] [--agent SLUG] [--limit N] [--json]

  vibe-agent run start --session ID [--model M] [--agent ID] [--json]
  vibe-agent run finish <id> --status completed|failed|cancelled [--steps N]
        [--error MSG] [--child ID] [--tokens-prompt N] [--tokens-completion N]
        [--cost-micros N] [--json]
  vibe-agent run cancel <id> [--json]
  vibe-agent run list --session ID [--limit N] [--active] [--stats] [--json]
`)
}

func withService(fn func(ctx context.Context, cfg config.Config, svc *agent.Service) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	return fn(ctx, cfg, agent.New(pool))
}

func runDB(args []string, w io.Writer) error {
	if len(args) == 0 {
		return usagef("db needs a subcommand: create, migrate or status")
	}
	switch args[0] {
	case "create":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		created, err := store.CreateDatabase(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintf(w, "created database %q on %s\n", cfg.Database, cfg.Host)
		} else {
			fmt.Fprintf(w, "database %q already exists on %s\n", cfg.Database, cfg.Host)
		}
		return nil
	case "migrate":
		return withService(func(ctx context.Context, cfg config.Config, svc *agent.Service) error {
			ran, err := store.Migrate(ctx, svc.Pool())
			if err != nil {
				return err
			}
			if len(ran) == 0 {
				fmt.Fprintf(w, "database %q is up to date\n", cfg.Database)
				return nil
			}
			for _, name := range ran {
				fmt.Fprintf(w, "applied %s\n", name)
			}
			return nil
		})
	case "status":
		return withService(func(ctx context.Context, cfg config.Config, svc *agent.Service) error {
			status, err := store.Status(ctx, svc.Pool())
			if err != nil {
				return err
			}
			pending, err := store.Pending(ctx, svc.Pool())
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "database %q on %s\n", cfg.Database, cfg.Host)
			if len(status) == 0 {
				fmt.Fprintln(w, "no migrations applied yet")
			}
			for _, s := range status {
				fmt.Fprintf(w, "  applied %s_%s at %s\n", s.Version, s.Name, s.AppliedAt.Format(time.RFC3339))
			}
			for _, m := range pending {
				fmt.Fprintf(w, "  pending %s_%s\n", m.Version, m.Name)
			}
			return nil
		})
	default:
		return usagef("unknown db subcommand %q", args[0])
	}
}

func runDoctor(w io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "config:   ok - database %q on %s\n", cfg.Database, cfg.Host)
	fmt.Fprintf(w, "dsn:      %s\n", redactDSN(cfg.DatabaseURL))

	return withService(func(ctx context.Context, cfg config.Config, svc *agent.Service) error {
		fmt.Fprintln(w, "database: reachable")

		status, err := store.Status(ctx, svc.Pool())
		if err != nil {
			return err
		}
		pending, err := store.Pending(ctx, svc.Pool())
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "schema:   %d applied, %d pending\n", len(status), len(pending))

		profiles, err := svc.Profiles(ctx, false)
		if err != nil {
			return err
		}
		builtin := 0
		for _, p := range profiles {
			if p.Builtin {
				builtin++
			}
		}
		fmt.Fprintf(w, "profiles: %d built-in, %d custom\n", builtin, len(profiles)-builtin)
		return nil
	})
}

func redactDSN(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "(unparseable)"
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.User(parsed.User.Username())
		}
	}
	return parsed.String()
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	flagArgs, positional := splitArgs(fs, args)
	if err := fs.Parse(flagArgs); err != nil {
		return nil, usagef("%v", err)
	}
	return positional, nil
}

// splitArgs accepts flags on either side of a positional argument. The
// standard flag package stops parsing at the first non-flag argument, which
// would make `session show <id> --turns` a parse error; this splitter moves
// the positionals aside so the documented syntax works.
func splitArgs(fs *flag.FlagSet, args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) > 1 && arg[0] == '-' {
			flagArgs = append(flagArgs, arg)
			if strings.ContainsRune(arg, '=') {
				continue
			}
			def := fs.Lookup(strings.TrimLeft(arg, "-"))
			if def == nil {
				continue
			}
			if boolFlag, ok := def.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
				continue
			}
			if i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, arg)
	}
	return flagArgs, positional
}

func setFlags(fs *flag.FlagSet) map[string]bool {
	out := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { out[f.Name] = true })
	return out
}

func idArg(raw, what string) (agent.ID, error) {
	id, err := agent.ParseID(raw)
	if err != nil {
		return "", usagef("%s is not a valid id: %q", what, raw)
	}
	return id, nil
}

func printJSON(w io.Writer, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", raw)
	return err
}

func preview(raw string, max int) string {
	raw = strings.ReplaceAll(raw, "\n", " ")
	runes := []rune(raw)
	if len(runes) <= max {
		return raw
	}
	return string(runes[:max]) + "..."
}

func partsFromFlags(text string, textSet bool, rawParts []string, partsFile string) ([]agent.Part, error) {
	if partsFile != "" {
		raw, err := os.ReadFile(partsFile)
		if err != nil {
			return nil, err
		}
		var parts []agent.Part
		if err := json.Unmarshal(raw, &parts); err != nil {
			return nil, usagef("%s does not hold a JSON array of parts: %v", partsFile, err)
		}
		return parts, nil
	}

	parts := make([]agent.Part, 0, len(rawParts)+1)
	if textSet {
		parts = append(parts, agent.Part{Type: "text", Text: text})
	}
	for _, raw := range rawParts {
		var part agent.Part
		if err := json.Unmarshal([]byte(raw), &part); err != nil {
			return nil, usagef("--part is not a JSON part: %v", err)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func metadataFromPairs(pairs []string) (map[string]any, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, usagef("--meta expects key=value, got %q", pair)
		}
		out[strings.TrimSpace(key)] = value
	}
	return out, nil
}

func permissionsFromJSON(raw string) (*agent.Ruleset, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, usagef("--permissions expects a JSON array of rules")
	}
	var rules agent.Ruleset
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, usagef("--permissions is not a JSON array of rules: %v", err)
	}
	return &rules, nil
}

// permissionsFromFlags reads the shell-friendly form
// `--permission allow:edit=*`, which avoids embedding JSON in an argument.
func permissionsFromFlags(values []string) (agent.Ruleset, error) {
	rules := make(agent.Ruleset, 0, len(values))
	for _, raw := range values {
		effectPart, rest, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, usagef("--permission expects EFFECT:ACTION=RESOURCE, got %q", raw)
		}
		effect, ok := parsePermissionEffect(effectPart)
		if !ok {
			return nil, usagef("--permission effect must be allow, ask or deny, got %q", effectPart)
		}
		action, resource, ok := strings.Cut(rest, "=")
		if !ok || strings.TrimSpace(action) == "" || strings.TrimSpace(resource) == "" {
			return nil, usagef("--permission expects EFFECT:ACTION=RESOURCE, got %q", raw)
		}
		rules = append(rules, agent.PermissionRule{
			Action:   strings.TrimSpace(action),
			Resource: strings.TrimSpace(resource),
			Effect:   effect,
		})
	}
	return rules, nil
}

func parsePermissionEffect(raw string) (agent.PermissionEffect, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "allow":
		return agent.EffectAllow, true
	case "ask":
		return agent.EffectAsk, true
	case "deny":
		return agent.EffectDeny, true
	default:
		return "", false
	}
}

func splitTools(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Split(raw, ",")
}
