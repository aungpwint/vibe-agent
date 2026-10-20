package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aungpwint/vibe-agent/agent"
	"github.com/aungpwint/vibe-agent/config"
)

func runTurn(args []string, w io.Writer) error {
	if len(args) == 0 {
		return usagef("turn needs a subcommand: append, list, complete or redact")
	}
	switch args[0] {
	case "append", "add":
		return turnAppend(args[1:], w)
	case "list":
		return turnList(args[1:], w)
	case "complete", "finish":
		return turnComplete(args[1:], w)
	case "redact":
		return turnRedact(args[1:], w)
	default:
		return usagef("unknown turn subcommand %q", args[0])
	}
}

func turnAppend(args []string, w io.Writer) error {
	fs := newFlagSet("turn append")
	session := fs.String("session", "", "session id (required)")
	role := fs.String("role", string(agent.TurnUser), "user, assistant, system or tool")
	status := fs.String("status", string(agent.TurnComplete), "streaming, complete, error or aborted")
	clientID := fs.String("client-id", "", "idempotency key")
	model := fs.String("model", "", "model name")
	runID := fs.String("run", "", "run id this turn belongs to")
	parentID := fs.String("parent", "", "turn id this turn replies to")
	text := fs.String("text", "", "text part")
	var parts stringList
	fs.Var(&parts, "part", "JSON part (repeatable)")
	partsFile := fs.String("parts-file", "", "JSON file holding an array of parts")
	promptTokens := fs.Int64("tokens-prompt", 0, "prompt tokens")
	completionTokens := fs.Int64("tokens-completion", 0, "completion tokens")
	costMicros := fs.Int64("cost-micros", 0, "cost in micros")
	asJSON := fs.Bool("json", false, "print JSON")
	if positional, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(positional) != 0 {
		return usagef("turn append takes flags, not positional arguments")
	}
	if *session == "" {
		return usagef("turn append needs --session")
	}
	sessionID, err := idArg(*session, "--session")
	if err != nil {
		return err
	}
	parsedRole, ok := agent.ParseTurnRole(*role)
	if !ok {
		return usagef("--role must be user, assistant, system or tool")
	}
	parsedStatus, ok := agent.ParseTurnStatus(*status)
	if !ok {
		return usagef("--status must be streaming, complete, error or aborted")
	}

	set := setFlags(fs)
	if !set["text"] && len(parts) == 0 && *partsFile == "" {
		return usagef("turn append needs --text, --part or --parts-file")
	}
	turnParts, err := partsFromFlags(*text, set["text"], parts, *partsFile)
	if err != nil {
		return err
	}

	params := agent.AppendTurnParams{
		SessionID: sessionID,
		Role:      parsedRole,
		Status:    parsedStatus,
		Parts:     turnParts,
		Tokens: agent.Tokens{
			Prompt:     *promptTokens,
			Completion: *completionTokens,
			CostMicros: *costMicros,
		},
	}
	if set["client-id"] {
		params.ClientID = clientID
	}
	if set["model"] {
		params.Model = model
	}
	if *runID != "" {
		id, err := idArg(*runID, "--run")
		if err != nil {
			return err
		}
		params.RunID = &id
	}
	if *parentID != "" {
		id, err := idArg(*parentID, "--parent")
		if err != nil {
			return err
		}
		params.ParentTurnID = &id
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		turn, err := svc.AppendTurn(ctx, params)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, turn)
		}
		printTurn(w, turn)
		return nil
	})
}

func turnList(args []string, w io.Writer) error {
	fs := newFlagSet("turn list")
	session := fs.String("session", "", "session id (required)")
	after := fs.Int64("after", 0, "only turns after this sequence")
	limit := fs.Int("limit", 50, "how many turns to show")
	role := fs.String("role", "", "filter by role")
	asJSON := fs.Bool("json", false, "print JSON")
	if positional, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(positional) != 0 {
		return usagef("turn list takes flags, not positional arguments")
	}
	if *session == "" {
		return usagef("turn list needs --session")
	}
	sessionID, err := idArg(*session, "--session")
	if err != nil {
		return err
	}

	query := agent.TurnQuery{SessionID: sessionID, AfterSequence: *after, Limit: *limit}
	if *role != "" {
		parsed, ok := agent.ParseTurnRole(*role)
		if !ok {
			return usagef("--role must be user, assistant, system or tool")
		}
		query.Role = &parsed
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		page, err := svc.Turns(ctx, query)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, page.Items)
		}
		if len(page.Items) == 0 {
			fmt.Fprintln(w, "no turns")
			return nil
		}
		printTurnHeader(w)
		for _, turn := range page.Items {
			printTurnLine(w, turn)
		}
		if page.HasMore {
			fmt.Fprintf(w, "(more turns exist; continue with --after %d)\n", page.NextSeq)
		}
		return nil
	})
}

func turnComplete(args []string, w io.Writer) error {
	fs := newFlagSet("turn complete")
	status := fs.String("status", string(agent.TurnComplete), "complete, error or aborted")
	text := fs.String("text", "", "replace the parts with this text")
	var parts stringList
	fs.Var(&parts, "part", "JSON part replacing the current parts (repeatable)")
	partsFile := fs.String("parts-file", "", "JSON file with the replacement parts")
	promptTokens := fs.Int64("tokens-prompt", 0, "prompt tokens")
	completionTokens := fs.Int64("tokens-completion", 0, "completion tokens")
	costMicros := fs.Int64("cost-micros", 0, "cost in micros")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent turn complete <id> [--status complete|error|aborted]")
	}
	id, err := idArg(positional[0], "turn")
	if err != nil {
		return err
	}
	parsedStatus, ok := agent.ParseTurnStatus(*status)
	if !ok {
		return usagef("--status must be complete, error or aborted")
	}
	if parsedStatus == agent.TurnStreaming {
		return usagef("--status must be complete, error or aborted")
	}

	set := setFlags(fs)
	params := agent.CompleteTurnParams{ID: id, Status: parsedStatus}
	if set["text"] || len(parts) > 0 || *partsFile != "" {
		replacement, err := partsFromFlags(*text, set["text"], parts, *partsFile)
		if err != nil {
			return err
		}
		params.Parts = replacement
	}
	if set["tokens-prompt"] || set["tokens-completion"] || set["cost-micros"] {
		params.Tokens = &agent.Tokens{
			Prompt:     *promptTokens,
			Completion: *completionTokens,
			CostMicros: *costMicros,
		}
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		turn, err := svc.CompleteTurn(ctx, params)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, turn)
		}
		printTurn(w, turn)
		return nil
	})
}

func turnRedact(args []string, w io.Writer) error {
	fs := newFlagSet("turn redact")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent turn redact <id>")
	}
	id, err := idArg(positional[0], "turn")
	if err != nil {
		return err
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		turn, err := svc.RedactTurn(ctx, id)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, turn)
		}
		printTurn(w, turn)
		return nil
	})
}

func runSearch(args []string, w io.Writer) error {
	fs := newFlagSet("search")
	session := fs.String("session", "", "limit the search to one session")
	role := fs.String("role", "", "filter by role")
	agentSlug := fs.String("agent", "", "filter by agent slug")
	limit := fs.Int("limit", 20, "how many matches to show")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(positional, " "))
	if text == "" {
		return usagef("usage: vibe-agent search TEXT [flags]")
	}

	query := agent.TurnSearch{Text: text, Limit: *limit}
	if *session != "" {
		id, err := idArg(*session, "--session")
		if err != nil {
			return err
		}
		query.SessionID = &id
	}
	if *role != "" {
		parsed, ok := agent.ParseTurnRole(*role)
		if !ok {
			return usagef("--role must be user, assistant, system or tool")
		}
		query.Role = &parsed
	}
	if *agentSlug != "" {
		query.AgentSlug = agentSlug
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		page, err := svc.SearchTurns(ctx, query)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, page.Items)
		}
		if len(page.Items) == 0 {
			fmt.Fprintln(w, "no matching turns")
			return nil
		}
		for _, turn := range page.Items {
			fmt.Fprintf(w, "%-26s #%-4d %-9s %s\n",
				turn.SessionID, turn.Sequence, turn.Role, preview(turnPreview(turn), 80))
		}
		if page.HasMore {
			fmt.Fprintln(w, "(more matches exist; raise --limit to see them)")
		}
		return nil
	})
}

func printTurnHeader(w io.Writer) {
	fmt.Fprintf(w, "%-5s %-26s %-9s %-9s %-12s %s\n",
		"SEQ", "ID", "ROLE", "STATUS", "TOKENS", "BODY")
}

func printTurnLine(w io.Writer, t agent.Turn) {
	tokens := fmt.Sprintf("%d/%d", t.Tokens.Prompt, t.Tokens.Completion)
	fmt.Fprintf(w, "%-5d %-26s %-9s %-9s %-12s %s\n",
		t.Sequence, t.ID, t.Role, t.Status, tokens, preview(turnPreview(t), 70))
}

func turnPreview(t agent.Turn) string {
	if t.IsRedacted() {
		return "(redacted)"
	}
	if t.Body != nil && strings.TrimSpace(*t.Body) != "" {
		return *t.Body
	}

	labels := make([]string, 0, len(t.Parts))
	for _, part := range t.Parts {
		switch part.Type {
		case "model":
			labels = append(labels, "model "+part.Model)
		case "agent":
			labels = append(labels, "agent "+part.Agent)
		case "tool":
			labels = append(labels, "tool "+part.Tool)
		case "file":
			labels = append(labels, "file")
		case "reasoning":
			labels = append(labels, "reasoning")
		case "snapshot":
			labels = append(labels, "snapshot")
		case "patch":
			labels = append(labels, "patch")
		case "step":
			labels = append(labels, "step")
		}
	}
	if len(labels) == 0 {
		return "(no text)"
	}
	return "[" + strings.Join(labels, ", ") + "]"
}

func printTurn(w io.Writer, t agent.Turn) {
	fmt.Fprintf(w, "id:        %s\n", t.ID)
	fmt.Fprintf(w, "session:   %s\n", t.SessionID)
	fmt.Fprintf(w, "sequence:  %d\n", t.Sequence)
	fmt.Fprintf(w, "role:      %s\n", t.Role)
	fmt.Fprintf(w, "status:    %s\n", t.Status)
	if t.Model != nil {
		fmt.Fprintf(w, "model:     %s\n", *t.Model)
	}
	if t.AgentID != nil {
		fmt.Fprintf(w, "agent:     %s\n", *t.AgentID)
	}
	if t.RunID != nil {
		fmt.Fprintf(w, "run:       %s\n", *t.RunID)
	}
	if t.ParentTurnID != nil {
		fmt.Fprintf(w, "parent:    %s\n", *t.ParentTurnID)
	}
	if t.ClientID != nil {
		fmt.Fprintf(w, "client:    %s\n", *t.ClientID)
	}
	if t.RedactedAt != nil {
		fmt.Fprintf(w, "redacted:  %s\n", t.RedactedAt.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(w, "tokens:    prompt %d, completion %d, cost %d micros\n",
		t.Tokens.Prompt, t.Tokens.Completion, t.Tokens.CostMicros)
	fmt.Fprintf(w, "created:   %s\n", t.CreatedAt.Local().Format("2006-01-02 15:04"))
	if t.CompletedAt != nil {
		fmt.Fprintf(w, "completed: %s\n", t.CompletedAt.Local().Format("2006-01-02 15:04"))
	}
	if t.Body != nil && *t.Body != "" {
		fmt.Fprintf(w, "body:\n%s\n", *t.Body)
	}
	for _, part := range t.Parts {
		label := part.Type
		switch part.Type {
		case "text", "reasoning":
			fmt.Fprintf(w, "[%s] %s\n", label, part.Text)
		case "tool":
			fmt.Fprintf(w, "[tool] %s status=%s call=%s\n", part.Tool, part.Status, part.CallID)
		case "agent":
			fmt.Fprintf(w, "[agent] %s\n", part.Agent)
		case "model":
			fmt.Fprintf(w, "[model] %s\n", part.Model)
		default:
			if len(part.Data) > 0 {
				fmt.Fprintf(w, "[%s] %s\n", label, part.Data)
			} else {
				fmt.Fprintf(w, "[%s]\n", label)
			}
		}
	}
}
