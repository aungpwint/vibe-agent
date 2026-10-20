package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aungpwint/vibe-agent/agent"
	"github.com/aungpwint/vibe-agent/config"
)

func runSession(args []string, w io.Writer) error {
	if len(args) == 0 {
		return usagef("session needs a subcommand: new, list, show, close, reopen, archive, unarchive, rm, fork, export or import")
	}
	switch args[0] {
	case "new":
		return sessionNew(args[1:], w)
	case "list":
		return sessionList(args[1:], w)
	case "show":
		return sessionShow(args[1:], w)
	case "close":
		return sessionStatus(args[1:], w, agent.SessionClosed)
	case "reopen":
		return sessionStatus(args[1:], w, agent.SessionOpen)
	case "archive":
		return sessionArchive(args[1:], w, true)
	case "unarchive":
		return sessionArchive(args[1:], w, false)
	case "rm", "delete":
		return sessionDelete(args[1:], w)
	case "fork":
		return sessionFork(args[1:], w)
	case "export":
		return sessionExport(args[1:], w)
	case "import":
		return sessionImport(args[1:], w)
	default:
		return usagef("unknown session subcommand %q", args[0])
	}
}

func sessionNew(args []string, w io.Writer) error {
	fs := newFlagSet("session new")
	title := fs.String("title", "", "session title (required)")
	agentSlug := fs.String("agent", "build", "agent slug")
	parent := fs.String("parent", "", "parent session id")
	model := fs.String("model", "", "model name")
	workspace := fs.String("workspace", "", "workspace path")
	project := fs.String("project", "", "project name")
	permissions := fs.String("permissions", "", "JSON array of permission rules")
	var meta stringList
	fs.Var(&meta, "meta", "metadata key=value (repeatable)")
	asJSON := fs.Bool("json", false, "print JSON")
	if positional, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(positional) != 0 {
		return usagef("session new takes flags, not positional arguments")
	}
	if strings.TrimSpace(*title) == "" {
		return usagef("session new needs --title")
	}

	params := agent.CreateSessionParams{Title: *title, AgentSlug: *agentSlug}
	if *parent != "" {
		id, err := idArg(*parent, "--parent")
		if err != nil {
			return err
		}
		params.ParentID = &id
	}
	if setFlags(fs)["model"] {
		params.Model = model
	}
	if setFlags(fs)["workspace"] {
		params.Workspace = workspace
	}
	if setFlags(fs)["project"] {
		params.Project = project
	}
	if setFlags(fs)["permissions"] {
		value, err := permissionsFromJSON(*permissions)
		if err != nil {
			return err
		}
		params.Permissions = value
	}
	value, err := metadataFromPairs(meta)
	if err != nil {
		return err
	}
	params.Metadata = value

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		session, err := svc.CreateSession(ctx, params)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, session)
		}
		printSession(w, session)
		return nil
	})
}

func sessionList(args []string, w io.Writer) error {
	fs := newFlagSet("session list")
	limit := fs.Int("limit", 20, "how many sessions to show")
	agentSlug := fs.String("agent", "", "filter by agent slug")
	status := fs.String("status", "", "filter by open or closed")
	parent := fs.String("parent", "", "filter by parent session id")
	root := fs.String("root", "", "filter by root session id")
	archived := fs.Bool("archived", false, "show archived sessions instead of active ones")
	all := fs.Bool("all", false, "show active and archived sessions")
	asJSON := fs.Bool("json", false, "print JSON")
	if positional, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(positional) != 0 {
		return usagef("session list takes flags, not positional arguments")
	}
	if *archived && *all {
		return usagef("--archived and --all are mutually exclusive")
	}

	query := agent.SessionQuery{}
	switch {
	case *archived:
		query.Archived = agent.ArchivedOnly
	case *all:
		query.Archived = agent.AllSessions
	default:
		query.Archived = agent.ActiveOnly
	}
	if *agentSlug != "" {
		query.AgentSlug = agentSlug
	}
	if *status != "" {
		parsed, ok := agent.ParseSessionStatus(*status)
		if !ok {
			return usagef("--status must be open or closed")
		}
		query.Status = &parsed
	}
	if *parent != "" {
		id, err := idArg(*parent, "--parent")
		if err != nil {
			return err
		}
		query.ParentID = &id
	}
	if *root != "" {
		id, err := idArg(*root, "--root")
		if err != nil {
			return err
		}
		query.RootID = &id
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		page, err := svc.Sessions(ctx, *limit, query)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, page.Items)
		}
		if len(page.Items) == 0 {
			fmt.Fprintln(w, "no sessions")
			return nil
		}
		printSessionHeader(w)
		for _, session := range page.Items {
			printSessionLine(w, session)
		}
		if page.HasNext {
			fmt.Fprintln(w, "(more sessions exist; raise --limit to see them)")
		}
		return nil
	})
}

func sessionShow(args []string, w io.Writer) error {
	fs := newFlagSet("session show")
	turns := fs.Bool("turns", false, "also print the transcript")
	limit := fs.Int("limit", 50, "how many turns to print with --turns")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent session show <id>")
	}
	id, err := idArg(positional[0], "session")
	if err != nil {
		return err
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		session, err := svc.Session(ctx, id)
		if err != nil {
			return err
		}
		if *asJSON && !*turns {
			return printJSON(w, session)
		}
		if !*asJSON {
			printSession(w, session)
		}
		if !*turns {
			return nil
		}

		page, err := svc.Turns(ctx, agent.TurnQuery{SessionID: id, Limit: *limit})
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, map[string]any{"session": session, "turns": page.Items})
		}
		fmt.Fprintln(w)
		printTurnHeader(w)
		for _, turn := range page.Items {
			printTurnLine(w, turn)
		}
		if page.HasMore {
			fmt.Fprintln(w, "(more turns exist; raise --limit to see them)")
		}
		return nil
	})
}

func sessionStatus(args []string, w io.Writer, status agent.SessionStatus) error {
	fs := newFlagSet("session status")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent session close|reopen <id>")
	}
	id, err := idArg(positional[0], "session")
	if err != nil {
		return err
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		var (
			session agent.Session
			err     error
		)
		if status == agent.SessionClosed {
			session, err = svc.CloseSession(ctx, id)
		} else {
			session, err = svc.ReopenSession(ctx, id)
		}
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, session)
		}
		printSession(w, session)
		return nil
	})
}

func sessionArchive(args []string, w io.Writer, archived bool) error {
	fs := newFlagSet("session archive")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent session archive|unarchive <id>")
	}
	id, err := idArg(positional[0], "session")
	if err != nil {
		return err
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		session, err := svc.SetSessionArchived(ctx, id, archived)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, session)
		}
		printSession(w, session)
		return nil
	})
}

func sessionDelete(args []string, w io.Writer) error {
	fs := newFlagSet("session rm")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent session rm <id>")
	}
	id, err := idArg(positional[0], "session")
	if err != nil {
		return err
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		if err := svc.DeleteSession(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(w, "deleted session %s\n", id)
		return nil
	})
}

func sessionFork(args []string, w io.Writer) error {
	fs := newFlagSet("session fork")
	at := fs.Int64("at", 0, "fork after this turn sequence")
	title := fs.String("title", "", "fork title")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent session fork <id> [--at N] [--title T]")
	}
	id, err := idArg(positional[0], "session")
	if err != nil {
		return err
	}

	params := agent.ForkSessionParams{ID: id}
	if setFlags(fs)["at"] {
		params.AtSequence = at
	}
	if setFlags(fs)["title"] {
		params.Title = title
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		session, err := svc.ForkSession(ctx, params)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, session)
		}
		printSession(w, session)
		return nil
	})
}

func sessionExport(args []string, w io.Writer) error {
	fs := newFlagSet("session export")
	out := fs.String("out", "", "write the document to this file instead of stdout")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent session export <id> [--out FILE]")
	}
	id, err := idArg(positional[0], "session")
	if err != nil {
		return err
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		doc, err := svc.ExportSession(ctx, id)
		if err != nil {
			return err
		}
		if *out == "" {
			return printJSON(w, doc)
		}
		raw, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(raw, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(w, "exported session %s to %s\n", id, *out)
		return nil
	})
}

func sessionImport(args []string, w io.Writer) error {
	fs := newFlagSet("session import")
	parent := fs.String("parent", "", "import as a child of this session id")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent session import FILE [--parent ID]")
	}
	file := positional[0]

	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc agent.Export
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s is not a vibe-agent export: %w", file, err)
	}
	if doc.Version != agent.ExportVersion {
		return fmt.Errorf("%s was exported with version %d; this tool reads version %d", file, doc.Version, agent.ExportVersion)
	}

	turns := make([]agent.ImportTurn, 0, len(doc.Turns))
	for _, t := range doc.Turns {
		turns = append(turns, agent.ImportTurn{
			Sequence:    t.Sequence,
			Role:        t.Role,
			Status:      t.Status,
			Parts:       t.Parts,
			Model:       t.Model,
			Tokens:      t.Tokens,
			CreatedAt:   t.CreatedAt,
			CompletedAt: t.CompletedAt,
		})
	}

	params := agent.ImportSessionParams{
		Title:       doc.Session.Title,
		AgentSlug:   doc.Session.AgentSlug,
		Model:       doc.Session.Model,
		Workspace:   doc.Session.Workspace,
		Project:     doc.Session.Project,
		Metadata:    doc.Session.Metadata,
		Permissions: doc.Session.Permissions,
		Turns:       turns,
	}
	if *parent != "" {
		id, err := idArg(*parent, "--parent")
		if err != nil {
			return err
		}
		params.ParentID = &id
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		session, err := svc.ImportSession(ctx, params)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, session)
		}
		printSession(w, session)
		return nil
	})
}

func printSessionHeader(w io.Writer) {
	fmt.Fprintf(w, "%-3s %-26s %-7s %-10s %5s %-16s %s\n",
		"", "ID", "STATUS", "AGENT", "TURNS", "LAST ACTIVITY", "TITLE")
}

func printSessionLine(w io.Writer, s agent.Session) {
	marker := "  "
	switch {
	case s.ArchivedAt != nil:
		marker = "A "
	case s.Status == agent.SessionClosed:
		marker = "C "
	case s.ParentID != nil:
		marker = "└ "
	}
	fmt.Fprintf(w, "%-3s %-26s %-7s %-10s %5d %-16s %s\n",
		marker, s.ID, s.Status, s.AgentSlug, s.TurnCount,
		s.LastActivityAt.Local().Format("2006-01-02 15:04"), preview(s.Title, 70))
}

func printSession(w io.Writer, s agent.Session) {
	fmt.Fprintf(w, "id:          %s\n", s.ID)
	fmt.Fprintf(w, "title:       %s\n", s.Title)
	fmt.Fprintf(w, "status:      %s\n", s.Status)
	fmt.Fprintf(w, "agent:       %s (%s)\n", s.AgentSlug, s.AgentID)
	if s.Model != nil {
		fmt.Fprintf(w, "model:       %s\n", *s.Model)
	}
	if s.ParentID != nil {
		fmt.Fprintf(w, "parent:      %s\n", *s.ParentID)
	}
	if s.RootID != nil {
		fmt.Fprintf(w, "root:        %s (depth %d)\n", *s.RootID, s.Depth)
	}
	if s.Workspace != nil {
		fmt.Fprintf(w, "workspace:   %s\n", *s.Workspace)
	}
	if s.Project != nil {
		fmt.Fprintf(w, "project:     %s\n", *s.Project)
	}
	if s.ForkedFromID != nil {
		fmt.Fprintf(w, "forked from: %s at sequence %d\n", *s.ForkedFromID, *s.ForkedFromSequence)
	}
	fmt.Fprintf(w, "turns:       %d (next sequence %d)\n", s.TurnCount, s.NextSequence)
	fmt.Fprintf(w, "tokens:      prompt %d, completion %d, cost %d micros\n",
		s.Tokens.Prompt, s.Tokens.Completion, s.Tokens.CostMicros)
	if s.ArchivedAt != nil {
		fmt.Fprintf(w, "archived:    %s\n", s.ArchivedAt.Local().Format("2006-01-02 15:04"))
	}
	if s.ClosedAt != nil {
		fmt.Fprintf(w, "closed:      %s\n", s.ClosedAt.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(w, "created:     %s\n", s.CreatedAt.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "activity:    %s\n", s.LastActivityAt.Local().Format("2006-01-02 15:04"))
	if len(s.Metadata) > 0 {
		raw, _ := json.Marshal(s.Metadata)
		fmt.Fprintf(w, "metadata:    %s\n", raw)
	}
	for _, rule := range s.Permissions {
		fmt.Fprintf(w, "permission:  %s %s %s\n", rule.Effect, rule.Action, rule.Resource)
	}
}
