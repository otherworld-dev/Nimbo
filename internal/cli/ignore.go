package cli

import (
	"context"
	"fmt"

	"github.com/otherworld/nimbo/internal/config"
)

// cmdIgnore manages the global ignore patterns applied to every sync pair.
//
//	nimbo ignore list
//	nimbo ignore add <glob>
//	nimbo ignore rm <glob>
func cmdIgnore(_ context.Context, args []string) error {
	d, err := dirs()
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "list" {
		return ignoreList(d)
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: nimbo ignore <list|add|rm> [glob]")
	}
	switch args[0] {
	case "add":
		return ignoreAdd(d, args[1])
	case "rm", "remove":
		return ignoreRemove(d, args[1])
	default:
		return fmt.Errorf("unknown ignore subcommand %q", args[0])
	}
}

func ignoreList(d config.Dirs) error {
	pats, err := d.LoadIgnore()
	if err != nil {
		return err
	}
	if len(pats) == 0 {
		fmt.Println("No global ignore patterns. (Built-in defaults like *.tmp still apply.)")
		return nil
	}
	for _, p := range pats {
		fmt.Println("  " + p)
	}
	return nil
}

func ignoreAdd(d config.Dirs, pattern string) error {
	pats, err := d.LoadIgnore()
	if err != nil {
		return err
	}
	for _, p := range pats {
		if p == pattern {
			return fmt.Errorf("pattern already present")
		}
	}
	pats = append(pats, pattern)
	if err := d.SaveIgnore(pats); err != nil {
		return err
	}
	fmt.Printf("Added global ignore: %s\n", pattern)
	return nil
}

func ignoreRemove(d config.Dirs, pattern string) error {
	pats, err := d.LoadIgnore()
	if err != nil {
		return err
	}
	out := pats[:0]
	for _, p := range pats {
		if p != pattern {
			out = append(out, p)
		}
	}
	if err := d.SaveIgnore(out); err != nil {
		return err
	}
	fmt.Printf("Removed global ignore: %s\n", pattern)
	return nil
}
