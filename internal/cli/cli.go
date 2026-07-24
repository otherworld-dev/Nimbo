// Package cli implements the nimbo command-line interface: argument
// dispatch and the individual subcommands. Keeping it out of package main makes
// the commands testable.
package cli

import (
	"context"
	"fmt"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/transport"
)

// Run dispatches a command line (os.Args[1:]).
func Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "login":
		return cmdLogin(ctx, rest)
	case "accounts":
		return cmdAccounts(ctx, rest)
	case "logout":
		return cmdLogout(ctx, rest)
	case "caps":
		return cmdCaps(ctx, rest)
	case "plan":
		return cmdPlan(ctx, rest)
	case "sync":
		return cmdSync(ctx, rest)
	case "watch":
		return cmdWatch(ctx, rest)
	case "notifications", "notes":
		return cmdNotifications(ctx, rest)
	case "pair":
		return cmdPair(ctx, rest)
	case "ignore":
		return cmdIgnore(ctx, rest)
	case "limit":
		return cmdLimit(ctx, rest)
	case "share":
		return cmdShare(ctx, rest)
	case "ls":
		return cmdLs(ctx, rest)
	case "get":
		return cmdGet(ctx, rest)
	case "put":
		return cmdPut(ctx, rest)
	case "rm":
		return cmdRm(ctx, rest)
	case "repair", "verify":
		return cmdRepair(ctx, rest)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Print(`nimbo — Nextcloud client (dev CLI)

Usage:
  nimbo login <server-url>     Authenticate via browser (Login Flow v2)
  nimbo accounts               List configured accounts
  nimbo logout [account-id]    Remove an account and its stored secret
  nimbo caps                   Show server capabilities (incl. notify_push)
  nimbo plan <local> [remote]  Dry-run: show what a sync would do
  nimbo sync <local> [remote]  Sync a local folder with a remote folder
  nimbo watch <local> [remote] Continuously sync + live notifications (Ctrl-C to stop)
  nimbo notifications          List current Nextcloud notifications
  nimbo pair add <local> [remote]   Configure a sync pair (used by the tray app)
  nimbo pair list / rm <index>      Manage configured sync pairs
  nimbo pair exclude <i> add <glob> Per-folder selective sync (exclude paths)
  nimbo ignore add/list/rm <glob>   Global ignore patterns
  nimbo limit up/down <kbps>        Bandwidth limits (KiB/s; 'none' to clear)
  nimbo share link <remote> [pass]  Create a public link (also: user/list/rm)
  nimbo ls [remote-path]       List a remote directory
  nimbo get <remote> [local]   Download a file
  nimbo put <local> <remote>   Upload a file
  nimbo rm <remote-path>       Delete a remote file or directory
  nimbo repair <local> [remote] [--apply] [--overwrite]
                               Verify the server against an authoritative LOCAL
                               copy (read-only by default). --apply uploads only
                               what's MISSING + creates missing folders;
                               --overwrite also fixes size mismatches. NEVER
                               deletes anything on the server.

Environment:
  NEXTCLIENT_DEBUG=1                 Verbose logging to stderr
`)
}

// dirs resolves the per-OS config/data directories.
func dirs() (config.Dirs, error) {
	return config.Resolve()
}

// loadStoreAt opens the account metadata store under already-resolved dirs.
func loadStoreAt(d config.Dirs) (*account.Store, error) {
	return account.LoadStore(d.AccountsFile())
}

// loadStore opens the account metadata store.
func loadStore() (*account.Store, config.Dirs, error) {
	d, err := dirs()
	if err != nil {
		return nil, config.Dirs{}, err
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return nil, config.Dirs{}, err
	}
	return st, d, nil
}

// clientForDefault builds an authenticated transport client for the default
// (first) account, loading the app password from the OS keychain.
func clientForDefault() (*transport.Client, account.Account, error) {
	st, _, err := loadStore()
	if err != nil {
		return nil, account.Account{}, err
	}
	acc, ok := st.Default()
	if !ok {
		return nil, account.Account{}, fmt.Errorf("no account configured — run: nimbo login <server-url>")
	}
	secret, err := account.LoadSecret(acc.ID)
	if err != nil {
		return nil, account.Account{}, err
	}
	return transport.New(acc.ServerURL, acc.LoginName, secret), acc, nil
}
