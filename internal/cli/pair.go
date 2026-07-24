package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/otherworld/nimbo/internal/config"
)

// cmdPair manages the persisted list of sync pairs that the tray/daemon syncs.
//
//	nimbo pair add <local-dir> [remote-dir]
//	nimbo pair list
//	nimbo pair rm <index>
func cmdPair(_ context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nimbo pair <add|list|rm> ...")
	}
	d, err := dirs()
	if err != nil {
		return err
	}
	// Pairs are per-account: scope to the active account (matching the engine)
	// so the CLI and GUI edit the same list.
	if st, serr := loadStoreAt(d); serr == nil {
		if acc, ok := st.Default(); ok {
			d = d.WithAccount(acc.ID)
			d.MigratePairs()
		}
	}
	switch args[0] {
	case "add":
		return pairAdd(d, args[1:])
	case "list", "ls":
		return pairList(d)
	case "rm", "remove":
		return pairRemove(d, args[1:])
	case "exclude":
		return pairExclude(d, args[1:])
	default:
		return fmt.Errorf("unknown pair subcommand %q (use add|list|rm|exclude)", args[0])
	}
}

// pairExclude manages a pair's selective-sync exclude patterns.
//
//	nimbo pair exclude <index> list
//	nimbo pair exclude <index> add <glob>
//	nimbo pair exclude <index> rm <glob>
func pairExclude(d config.Dirs, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: nimbo pair exclude <index> <list|add|rm> [glob]")
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("index must be a number (see 'pair list')")
	}
	pairs, err := d.LoadPairs()
	if err != nil {
		return err
	}
	if idx < 0 || idx >= len(pairs) {
		return fmt.Errorf("no pair at index %d", idx)
	}

	switch args[1] {
	case "list":
		if len(pairs[idx].Excludes) == 0 {
			fmt.Println("No excludes for this pair.")
			return nil
		}
		for _, e := range pairs[idx].Excludes {
			fmt.Println("  " + e)
		}
		return nil
	case "add":
		if len(args) != 3 {
			return fmt.Errorf("usage: nimbo pair exclude <index> add <glob>")
		}
		pairs[idx].Excludes = append(pairs[idx].Excludes, args[2])
	case "rm", "remove":
		if len(args) != 3 {
			return fmt.Errorf("usage: nimbo pair exclude <index> rm <glob>")
		}
		out := pairs[idx].Excludes[:0]
		for _, e := range pairs[idx].Excludes {
			if e != args[2] {
				out = append(out, e)
			}
		}
		pairs[idx].Excludes = out
	default:
		return fmt.Errorf("unknown subcommand %q (use list|add|rm)", args[1])
	}
	if err := d.SavePairs(pairs); err != nil {
		return err
	}
	fmt.Printf("Updated excludes for [%d] %s\n", idx, pairs[idx].LocalDir)
	return nil
}

func pairAdd(d config.Dirs, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: nimbo pair add <local-dir> [remote-dir]")
	}
	local, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	remote := ""
	if len(args) == 2 {
		remote = strings.Trim(args[1], "/")
	}

	pairs, err := d.LoadPairs()
	if err != nil {
		return err
	}
	for _, p := range pairs {
		if p.LocalDir == local && p.RemoteRoot == remote {
			return fmt.Errorf("that pair is already configured")
		}
	}
	pairs = append(pairs, config.SyncPair{LocalDir: local, RemoteRoot: remote})
	if err := d.SavePairs(pairs); err != nil {
		return err
	}
	fmt.Printf("Added sync pair:\n  %s  <->  remote:%s\n", local, remote)
	return nil
}

func pairList(d config.Dirs) error {
	pairs, err := d.LoadPairs()
	if err != nil {
		return err
	}
	if len(pairs) == 0 {
		fmt.Println("No sync pairs configured. Add one: nimbo pair add <local-dir> [remote-dir]")
		return nil
	}
	for i, p := range pairs {
		fmt.Printf("[%d] %s  <->  remote:%s\n", i, p.LocalDir, p.RemoteRoot)
	}
	return nil
}

func pairRemove(d config.Dirs, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: nimbo pair rm <index>")
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("index must be a number (see 'pair list')")
	}
	pairs, err := d.LoadPairs()
	if err != nil {
		return err
	}
	if idx < 0 || idx >= len(pairs) {
		return fmt.Errorf("no pair at index %d (see 'pair list')", idx)
	}
	removed := pairs[idx]
	pairs = append(pairs[:idx], pairs[idx+1:]...)
	if err := d.SavePairs(pairs); err != nil {
		return err
	}
	fmt.Printf("Removed: %s  <->  remote:%s\n", removed.LocalDir, removed.RemoteRoot)
	return nil
}
