package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/engine"
)

// cmdPlan prints the reconciliation plan for a sync pair without applying it.
//
//	nimbo plan <local-dir> [remote-dir]
func cmdPlan(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: nimbo plan <local-dir> [remote-dir]")
	}
	p := agent.Pair{LocalDir: args[0]}
	if len(args) == 2 {
		p.RemoteRoot = strings.Trim(args[1], "/")
	}

	eng, err := agent.NewEngine(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Planning %s  <->  %s/%s\n\n", p.LocalDir, eng.Account.ServerURL, p.RemoteRoot)

	actions, err := eng.Plan(ctx, p)
	if err != nil {
		return err
	}
	printPlan(actions)
	if len(actions) > 0 {
		fmt.Println("(dry run — nothing was changed; run 'sync' to apply)")
	}
	return nil
}

// printPlan renders the action list grouped by kind with a one-line summary.
func printPlan(actions []engine.Action) {
	if len(actions) == 0 {
		fmt.Println("Already in sync — nothing to do.")
		return
	}

	counts := map[engine.ActionKind]int{}
	for _, a := range actions {
		counts[a.Kind]++
	}

	order := []engine.ActionKind{
		engine.ActMoveLocal, engine.ActMoveRemote,
		engine.ActDownload, engine.ActUpload,
		engine.ActCreateLocalDir, engine.ActCreateRemoteDir,
		engine.ActDeleteLocal, engine.ActDeleteRemote,
		engine.ActConflict,
	}

	for _, k := range order {
		if counts[k] == 0 {
			continue
		}
		fmt.Printf("== %s (%d) ==\n", k, counts[k])
		for _, a := range actions {
			if a.Kind == k {
				if a.Dest != "" {
					fmt.Printf("  %-30s -> %s  %s\n", a.Path, a.Dest, a.Reason)
				} else {
					fmt.Printf("  %-30s  %s\n", a.Path, a.Reason)
				}
			}
		}
		fmt.Println()
	}

	var parts []string
	for _, k := range order {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	fmt.Printf("Plan: %s\n", strings.Join(parts, ", "))
	if counts[engine.ActConflict] > 0 {
		fmt.Println("Note: conflicts are auto-resolved — identical content is merged, otherwise both versions are kept.")
	}
}
