package main

import (
	"context"
	"fmt"
	"io"

	"github.com/aungpwint/vibe-agent/agent"
	"github.com/aungpwint/vibe-agent/config"
)

func runRun(args []string, w io.Writer) error {
	if len(args) == 0 {
		return usagef("run needs a subcommand: start, finish, cancel or list")
	}
	switch args[0] {
	case "start":
		return runStart(args[1:], w)
	case "finish":
		return runFinish(args[1:], w)
	case "cancel":
		return runCancel(args[1:], w)
	case "list", "ls":
		return runList(args[1:], w)
	default:
		return usagef("unknown run subcommand %q", args[0])
	}
}

func runStart(args []string, w io.Writer) error {
	fs := newFlagSet("run start")
	session := fs.String("session", "", "session id (required)")
	model := fs.String("model", "", "model name")
	agentID := fs.String("agent", "", "agent profile id")
	asJSON := fs.Bool("json", false, "print JSON")
	if positional, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(positional) != 0 {
		return usagef("run start takes flags, not positional arguments")
	}
	if *session == "" {
		return usagef("run start needs --session")
	}
	sessionID, err := idArg(*session, "--session")
	if err != nil {
		return err
	}

	params := agent.StartRunParams{SessionID: sessionID}
	if setFlags(fs)["model"] {
		params.Model = model
	}
	if *agentID != "" {
		id, err := idArg(*agentID, "--agent")
		if err != nil {
			return err
		}
		params.AgentID = &id
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		run, err := svc.StartRun(ctx, params)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, run)
		}
		printRun(w, run)
		return nil
	})
}

func runFinish(args []string, w io.Writer) error {
	fs := newFlagSet("run finish")
	status := fs.String("status", "", "completed, failed or cancelled (required)")
	steps := fs.Int("steps", 0, "how many steps the run took")
	message := fs.String("error", "", "error message (failed runs)")
	child := fs.String("child", "", "child session id this run created")
	promptTokens := fs.Int64("tokens-prompt", 0, "prompt tokens")
	completionTokens := fs.Int64("tokens-completion", 0, "completion tokens")
	costMicros := fs.Int64("cost-micros", 0, "cost in micros")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent run finish <id> --status completed|failed|cancelled")
	}
	id, err := idArg(positional[0], "run")
	if err != nil {
		return err
	}
	if *status == "" {
		return usagef("run finish needs --status completed, failed or cancelled")
	}
	parsedStatus, ok := agent.ParseRunStatus(*status)
	if !ok || !parsedStatus.Terminal() {
		return usagef("--status must be completed, failed or cancelled")
	}

	params := agent.FinishRunParams{
		ID:     id,
		Status: parsedStatus,
		Steps:  *steps,
		Tokens: agent.Tokens{
			Prompt:     *promptTokens,
			Completion: *completionTokens,
			CostMicros: *costMicros,
		},
	}
	if setFlags(fs)["error"] {
		params.Error = message
	}
	if *child != "" {
		childID, err := idArg(*child, "--child")
		if err != nil {
			return err
		}
		params.ChildSessionID = &childID
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		run, err := svc.FinishRun(ctx, params)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, run)
		}
		printRun(w, run)
		return nil
	})
}

func runCancel(args []string, w io.Writer) error {
	fs := newFlagSet("run cancel")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent run cancel <id>")
	}
	id, err := idArg(positional[0], "run")
	if err != nil {
		return err
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		run, err := svc.CancelRun(ctx, id)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, run)
		}
		printRun(w, run)
		return nil
	})
}

func runList(args []string, w io.Writer) error {
	fs := newFlagSet("run list")
	session := fs.String("session", "", "session id (required)")
	limit := fs.Int("limit", 20, "how many runs to show")
	active := fs.Bool("active", false, "show only queued and running runs")
	stats := fs.Bool("stats", false, "print run statistics instead of the runs")
	asJSON := fs.Bool("json", false, "print JSON")
	if positional, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(positional) != 0 {
		return usagef("run list takes flags, not positional arguments")
	}
	if *session == "" {
		return usagef("run list needs --session")
	}
	sessionID, err := idArg(*session, "--session")
	if err != nil {
		return err
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		if *stats {
			value, err := svc.RunStats(ctx, sessionID)
			if err != nil {
				return err
			}
			if *asJSON {
				return printJSON(w, value)
			}
			printRunStats(w, value)
			return nil
		}

		var runs []agent.Run
		if *active {
			runs, err = svc.ActiveRuns(ctx, sessionID)
		} else {
			runs, err = svc.Runs(ctx, sessionID, *limit)
		}
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, runs)
		}
		if len(runs) == 0 {
			fmt.Fprintln(w, "no runs")
			return nil
		}
		printRunHeader(w)
		for _, run := range runs {
			printRunLine(w, run)
		}
		return nil
	})
}

func printRunHeader(w io.Writer) {
	fmt.Fprintf(w, "%-26s %-10s %-16s %6s %-12s %s\n",
		"ID", "STATUS", "MODEL", "STEPS", "TOKENS", "CREATED")
}

func printRunLine(w io.Writer, r agent.Run) {
	model := "-"
	if r.Model != nil {
		model = preview(*r.Model, 16)
	}
	tokens := fmt.Sprintf("%d/%d", r.Tokens.Prompt, r.Tokens.Completion)
	fmt.Fprintf(w, "%-26s %-10s %-16s %6d %-12s %s\n",
		r.ID, r.Status, model, r.Steps, tokens, r.CreatedAt.Local().Format("2006-01-02 15:04"))
}

func printRun(w io.Writer, r agent.Run) {
	fmt.Fprintf(w, "id:        %s\n", r.ID)
	fmt.Fprintf(w, "session:   %s\n", r.SessionID)
	fmt.Fprintf(w, "status:    %s\n", r.Status)
	if r.AgentID != nil {
		fmt.Fprintf(w, "agent:     %s\n", *r.AgentID)
	}
	if r.Model != nil {
		fmt.Fprintf(w, "model:     %s\n", *r.Model)
	}
	if r.ChildSessionID != nil {
		fmt.Fprintf(w, "child:     %s\n", *r.ChildSessionID)
	}
	fmt.Fprintf(w, "steps:     %d\n", r.Steps)
	fmt.Fprintf(w, "tokens:    prompt %d, completion %d, cost %d micros\n",
		r.Tokens.Prompt, r.Tokens.Completion, r.Tokens.CostMicros)
	if r.Error != nil {
		fmt.Fprintf(w, "error:     %s\n", *r.Error)
	}
	fmt.Fprintf(w, "created:   %s\n", r.CreatedAt.Local().Format("2006-01-02 15:04"))
	if r.StartedAt != nil {
		fmt.Fprintf(w, "started:   %s\n", r.StartedAt.Local().Format("2006-01-02 15:04"))
	}
	if r.FinishedAt != nil {
		fmt.Fprintf(w, "finished:  %s\n", r.FinishedAt.Local().Format("2006-01-02 15:04"))
	}
}

func printRunStats(w io.Writer, s agent.RunStats) {
	fmt.Fprintf(w, "runs:      %d total, %d queued, %d running, %d completed, %d failed, %d cancelled\n",
		s.Total, s.Queued, s.Running, s.Completed, s.Failed, s.Cancelled)
	fmt.Fprintf(w, "steps:     %d\n", s.Steps)
	fmt.Fprintf(w, "tokens:    prompt %d, completion %d, cost %d micros\n",
		s.Tokens.Prompt, s.Tokens.Completion, s.Tokens.CostMicros)
}
