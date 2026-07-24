package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/transfer"
)

// cmdSync reconciles a local folder with a remote folder once and exits.
//
//	nimbo sync <local-dir> [remote-dir]
func cmdSync(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: nimbo sync <local-dir> [remote-dir]")
	}
	p := agent.Pair{LocalDir: args[0]}
	if len(args) == 2 {
		p.RemoteRoot = strings.Trim(args[1], "/")
	}

	eng, err := agent.NewEngine(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Syncing %s  <->  %s/%s\n", p.LocalDir, eng.Account.ServerURL, p.RemoteRoot)

	stats, err := eng.SyncOnce(ctx, p)
	if err != nil {
		return err
	}
	if stats == (transfer.Stats{}) {
		fmt.Println("Already in sync — nothing to do.")
		return nil
	}
	printStats(stats)
	return nil
}

// printStats renders an executor result summary.
func printStats(s transfer.Stats) {
	if s == (transfer.Stats{}) {
		return
	}
	fmt.Printf("Synced: %d down, %d up, %d moved, %d local dirs, %d remote dirs, %d del-local, %d del-remote.\n",
		s.Downloaded, s.Uploaded, s.Moved, s.MkLocal, s.MkRemote, s.DelLocal, s.DelRemote)
	if s.ConflictsIdentical > 0 {
		fmt.Printf("  %d conflict(s) were false alarms (identical content), merged automatically.\n", s.ConflictsIdentical)
	}
	if s.ConflictsResurrected > 0 {
		fmt.Printf("  %d delete-vs-edit conflict(s) resolved by keeping the edited version.\n", s.ConflictsResurrected)
	}
	if s.Conflicts > 0 {
		fmt.Printf("  %d conflict(s) kept both versions (see \"conflicted copy\" files).\n", s.Conflicts)
	}
	if s.Failed > 0 {
		fmt.Printf("  %d operation(s) failed — see logs. Re-run to retry.\n", s.Failed)
	}
}
