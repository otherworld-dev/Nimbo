package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/transport"
)

// cmdLogin runs the interactive Login Flow v2 against a server URL.
func cmdLogin(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: nimbo login <server-url>")
	}
	server := normalizeServer(args[0])

	flow, err := account.InitLogin(ctx, server)
	if err != nil {
		return err
	}

	fmt.Println("Open this URL in your browser to authorise Nimbo:")
	fmt.Println("  " + flow.LoginURL)
	if err := openBrowser(flow.LoginURL); err == nil {
		fmt.Println("(attempted to open it for you)")
	}
	fmt.Println("\nWaiting for approval… (Ctrl-C to cancel)")

	creds, err := flow.Poll(ctx)
	if err != nil {
		return err
	}

	st, _, err := loadStore()
	if err != nil {
		return err
	}
	acc, err := account.Complete(st, creds)
	if err != nil {
		return err
	}

	fmt.Printf("\nLogged in as %s on %s\n", acc.LoginName, acc.ServerURL)
	fmt.Printf("Account id: %s\n", acc.ID)

	// Confirm the credential works and surface real-time push availability.
	client := transport.New(acc.ServerURL, acc.LoginName, creds.AppPassword)
	if caps, err := client.FetchCapabilities(ctx); err == nil {
		fmt.Printf("Server: Nextcloud %s\n", caps.Version.String)
		if caps.NotifyPush != nil {
			fmt.Println("Real-time push: available (notify_push)")
		} else {
			fmt.Println("Real-time push: not available (notify_push app not installed)")
		}
	}
	return nil
}

// cmdAccounts lists configured accounts.
func cmdAccounts(_ context.Context, _ []string) error {
	st, _, err := loadStore()
	if err != nil {
		return err
	}
	if len(st.Accounts) == 0 {
		fmt.Println("No accounts configured. Run: nimbo login <server-url>")
		return nil
	}
	for i, a := range st.Accounts {
		marker := " "
		if i == 0 {
			marker = "*" // default account
		}
		fmt.Printf("%s %s  %s  (%s)\n", marker, a.LoginName, a.ServerURL, a.ID)
	}
	return nil
}

// cmdLogout removes an account and its keychain secret.
func cmdLogout(_ context.Context, args []string) error {
	st, _, err := loadStore()
	if err != nil {
		return err
	}
	var acc account.Account
	var ok bool
	if len(args) == 1 {
		acc, ok = st.Find(args[0])
	} else {
		acc, ok = st.Default()
	}
	if !ok {
		return fmt.Errorf("account not found")
	}
	if err := account.DeleteSecret(acc.ID); err != nil {
		return err
	}
	if err := st.Remove(acc.ID); err != nil {
		return err
	}
	fmt.Printf("Removed account %s (%s)\n", acc.LoginName, acc.ID)
	return nil
}

// cmdRm deletes a remote file or directory (recursively).
func cmdRm(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: nimbo rm <remote-path>")
	}
	remote := strings.Trim(args[0], "/")
	if remote == "" {
		return fmt.Errorf("refusing to delete the files root")
	}
	client, _, err := clientForDefault()
	if err != nil {
		return err
	}
	if err := client.Delete(ctx, remote); err != nil {
		return err
	}
	fmt.Printf("Deleted %s\n", remote)
	return nil
}

// cmdCaps prints server capabilities for the default account.
func cmdCaps(ctx context.Context, _ []string) error {
	client, acc, err := clientForDefault()
	if err != nil {
		return err
	}
	caps, err := client.FetchCapabilities(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Account:        %s @ %s\n", acc.LoginName, acc.ServerURL)
	fmt.Printf("Server version: Nextcloud %s\n", caps.Version.String)
	if caps.NotifyPush != nil {
		fmt.Println("notify_push:    available")
		fmt.Printf("  websocket: %s\n", caps.NotifyPush.Websocket)
		fmt.Printf("  pre_auth:  %s\n", caps.NotifyPush.PreAuth)
	} else {
		fmt.Println("notify_push:    not available")
	}
	return nil
}

// cmdLs lists a remote directory (depth 1).
func cmdLs(ctx context.Context, args []string) error {
	remote := ""
	if len(args) >= 1 {
		remote = strings.Trim(args[0], "/")
	}
	client, _, err := clientForDefault()
	if err != nil {
		return err
	}
	entries, err := client.PropFind(ctx, remote, 1)
	if err != nil {
		return err
	}

	// Drop the directory's own entry; sort dirs first, then by name.
	children := entries[:0]
	for _, e := range entries {
		if strings.Trim(e.Path, "/") == remote {
			continue
		}
		children = append(children, e)
	}
	sort.Slice(children, func(i, j int) bool {
		if children[i].IsDir != children[j].IsDir {
			return children[i].IsDir
		}
		return children[i].Path < children[j].Path
	})

	if len(children) == 0 {
		fmt.Println("(empty)")
		return nil
	}
	for _, e := range children {
		name := path.Base(e.Path)
		if e.IsDir {
			fmt.Printf("d %10s  %s/\n", "-", name)
		} else {
			fmt.Printf("- %10s  %s\n", humanSize(e.Size), name)
		}
	}
	return nil
}

// cmdGet downloads a remote file to a local path (default: its base name).
func cmdGet(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: nimbo get <remote> [local]")
	}
	remote := strings.Trim(args[0], "/")
	local := path.Base(remote)
	if len(args) >= 2 {
		local = args[1]
	}
	client, _, err := clientForDefault()
	if err != nil {
		return err
	}

	body, _, err := client.Get(ctx, remote)
	if err != nil {
		return err
	}
	defer body.Close()

	f, err := os.Create(local)
	if err != nil {
		return err
	}
	defer f.Close()

	n, err := io.Copy(f, body)
	if err != nil {
		return fmt.Errorf("download %s: %w", remote, err)
	}
	fmt.Printf("Downloaded %s -> %s (%s)\n", remote, local, humanSize(n))
	return nil
}

// cmdPut uploads a local file to a remote path with a single PUT.
func cmdPut(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: nimbo put <local> <remote>")
	}
	local := args[0]
	remote := strings.Trim(args[1], "/")

	client, _, err := clientForDefault()
	if err != nil {
		return err
	}

	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	etag, err := client.Put(ctx, remote, f, info.Size())
	if err != nil {
		return err
	}
	fmt.Printf("Uploaded %s -> %s (%s, etag %s)\n", filepath.Base(local), remote, humanSize(info.Size()), etag)
	return nil
}
