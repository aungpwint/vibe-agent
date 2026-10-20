package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/aungpwint/vibe-agent/agent"
	"github.com/aungpwint/vibe-agent/config"
)

func runProfile(args []string, w io.Writer) error {
	if len(args) == 0 {
		return usagef("profile needs a subcommand: list, show, create, update, archive or restore")
	}
	switch args[0] {
	case "list":
		return profileList(args[1:], w)
	case "show":
		return profileShow(args[1:], w)
	case "create":
		return profileCreate(args[1:], w)
	case "update":
		return profileUpdate(args[1:], w)
	case "archive":
		return profileArchive(args[1:], w, true)
	case "restore":
		return profileArchive(args[1:], w, false)
	default:
		return usagef("unknown profile subcommand %q", args[0])
	}
}

func profileList(args []string, w io.Writer) error {
	fs := newFlagSet("profile list")
	all := fs.Bool("all", false, "include archived profiles")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		profiles, err := svc.Profiles(ctx, *all)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, profiles)
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "SLUG\tMODE\tNAME\tID\tNOTES")
		for _, p := range profiles {
			notes := ""
			if p.Builtin {
				notes = "built-in"
			}
			if p.ArchivedAt != nil {
				notes = strings.TrimSpace(notes + " archived")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Slug, p.Mode, p.Name, p.ID, notes)
		}
		return tw.Flush()
	})
}

func profileShow(args []string, w io.Writer) error {
	fs := newFlagSet("profile show")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent profile show <id|slug>")
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		profile, err := svc.ResolveProfile(ctx, positional[0])
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, profile)
		}
		fmt.Fprintf(w, "id:          %s\n", profile.ID)
		fmt.Fprintf(w, "slug:        %s\n", profile.Slug)
		fmt.Fprintf(w, "name:        %s\n", profile.Name)
		fmt.Fprintf(w, "mode:        %s\n", profile.Mode)
		fmt.Fprintf(w, "built-in:    %t\n", profile.Builtin)
		if profile.Model != nil {
			fmt.Fprintf(w, "model:       %s\n", *profile.Model)
		}
		if profile.Temperature != nil {
			fmt.Fprintf(w, "temperature: %g\n", *profile.Temperature)
		}
		if profile.Description != "" {
			fmt.Fprintf(w, "description: %s\n", profile.Description)
		}
		if len(profile.Tools) > 0 {
			fmt.Fprintf(w, "tools:       %s\n", strings.Join(profile.Tools, ", "))
		}
		if profile.ArchivedAt != nil {
			fmt.Fprintf(w, "archived:    %s\n", profile.ArchivedAt.Format("2006-01-02 15:04"))
		}
		fmt.Fprintf(w, "created:     %s\n", profile.CreatedAt.Format("2006-01-02 15:04"))
		for _, rule := range profile.Permissions {
			fmt.Fprintf(w, "permission:  %s %s %s\n", rule.Effect, rule.Action, rule.Resource)
		}
		return nil
	})
}

func profileCreate(args []string, w io.Writer) error {
	fs := newFlagSet("profile create")
	slug := fs.String("slug", "", "unique slug (required)")
	name := fs.String("name", "", "display name (required)")
	description := fs.String("description", "", "description")
	mode := fs.String("mode", string(agent.ModeAll), "primary, subagent or all")
	model := fs.String("model", "", "default model")
	prompt := fs.String("prompt", "", "system prompt")
	temperature := fs.Float64("temperature", 0, "sampling temperature (0-2)")
	tools := fs.String("tools", "", "comma-separated tool names")
	permissions := fs.String("permissions", "", "JSON array of permission rules")
	var meta stringList
	fs.Var(&meta, "meta", "metadata key=value (repeatable)")
	var permissionFlags stringList
	fs.Var(&permissionFlags, "permission", "EFFECT:ACTION=RESOURCE, e.g. allow:edit=* (repeatable)")
	asJSON := fs.Bool("json", false, "print JSON")
	if positional, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(positional) != 0 {
		return usagef("profile create takes flags, not positional arguments")
	}
	if strings.TrimSpace(*slug) == "" || strings.TrimSpace(*name) == "" {
		return usagef("profile create needs --slug and --name")
	}
	parsedMode, ok := agent.ParseAgentMode(*mode)
	if !ok {
		return usagef("--mode must be primary, subagent or all")
	}
	metadata, err := metadataFromPairs(meta)
	if err != nil {
		return err
	}
	set := setFlags(fs)
	var perms *agent.Ruleset
	if set["permissions"] {
		perms, err = permissionsFromJSON(*permissions)
		if err != nil {
			return err
		}
	}

	params := agent.CreateProfileParams{
		Slug:        *slug,
		Name:        *name,
		Description: *description,
		Mode:        parsedMode,
		Tools:       splitTools(*tools),
		Metadata:    metadata,
	}
	if set["model"] {
		params.Model = model
	}
	if set["prompt"] {
		params.SystemPrompt = prompt
	}
	if set["temperature"] {
		params.Temperature = temperature
	}
	if perms != nil && len(permissionFlags) > 0 {
		return usagef("use either --permissions or --permission, not both")
	}
	if len(permissionFlags) > 0 {
		value, err := permissionsFromFlags(permissionFlags)
		if err != nil {
			return err
		}
		params.Permissions = value
	} else if perms != nil {
		params.Permissions = *perms
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		profile, err := svc.CreateProfile(ctx, params)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, profile)
		}
		fmt.Fprintf(w, "created agent %s (%s)\n", profile.Slug, profile.ID)
		return nil
	})
}

func profileUpdate(args []string, w io.Writer) error {
	fs := newFlagSet("profile update")
	name := fs.String("name", "", "display name")
	description := fs.String("description", "", "description")
	mode := fs.String("mode", "", "primary, subagent or all")
	model := fs.String("model", "", "default model")
	prompt := fs.String("prompt", "", "system prompt")
	temperature := fs.Float64("temperature", 0, "sampling temperature (0-2)")
	tools := fs.String("tools", "", "comma-separated tool names")
	permissions := fs.String("permissions", "", "JSON array of permission rules")
	var meta stringList
	fs.Var(&meta, "meta", "metadata key=value (repeatable)")
	var permissionFlags stringList
	fs.Var(&permissionFlags, "permission", "EFFECT:ACTION=RESOURCE, e.g. allow:edit=* (repeatable)")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent profile update <id|slug> [flags]")
	}

	set := setFlags(fs)
	if !set["name"] && !set["description"] && !set["mode"] && !set["model"] &&
		!set["prompt"] && !set["temperature"] && !set["tools"] && !set["permissions"] &&
		len(permissionFlags) == 0 && !set["meta"] {
		return usagef("profile update needs at least one field flag")
	}

	params := agent.UpdateProfileParams{}
	if set["name"] {
		params.Name = name
	}
	if set["description"] {
		params.Description = description
	}
	if set["mode"] {
		parsed, ok := agent.ParseAgentMode(*mode)
		if !ok {
			return usagef("--mode must be primary, subagent or all")
		}
		params.Mode = &parsed
	}
	if set["model"] {
		params.Model = model
	}
	if set["prompt"] {
		params.SystemPrompt = prompt
	}
	if set["temperature"] {
		params.Temperature = temperature
	}
	if set["tools"] {
		value := splitTools(*tools)
		params.Tools = &value
	}
	if set["permissions"] && len(permissionFlags) > 0 {
		return usagef("use either --permissions or --permission, not both")
	}
	if len(permissionFlags) > 0 {
		value, err := permissionsFromFlags(permissionFlags)
		if err != nil {
			return err
		}
		params.Permissions = &value
	} else if set["permissions"] {
		value, err := permissionsFromJSON(*permissions)
		if err != nil {
			return err
		}
		params.Permissions = value
	}
	if set["meta"] {
		value, err := metadataFromPairs(meta)
		if err != nil {
			return err
		}
		params.Metadata = &value
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		current, err := svc.ResolveProfile(ctx, positional[0])
		if err != nil {
			return err
		}
		params.ID = current.ID
		profile, err := svc.UpdateProfile(ctx, params)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, profile)
		}
		fmt.Fprintf(w, "updated agent %s (%s)\n", profile.Slug, profile.ID)
		return nil
	})
}

func profileArchive(args []string, w io.Writer, archive bool) error {
	fs := newFlagSet("profile archive")
	asJSON := fs.Bool("json", false, "print JSON")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: vibe-agent profile archive|restore <id|slug>")
	}

	return withService(func(ctx context.Context, _ config.Config, svc *agent.Service) error {
		var (
			profile agent.Profile
			err     error
		)
		if archive {
			profile, err = svc.ArchiveProfile(ctx, positional[0])
		} else {
			profile, err = svc.RestoreProfile(ctx, positional[0])
		}
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(w, profile)
		}
		action := "archived"
		if !archive {
			action = "restored"
		}
		fmt.Fprintf(w, "%s agent %s (%s)\n", action, profile.Slug, profile.ID)
		return nil
	})
}
