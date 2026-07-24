package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/otherworld/nimbo/internal/transport"
)

// cmdShare manages shares on remote paths.
//
//	nimbo share link <remote-path> [password]   create a public link
//	nimbo share user <remote-path> <user>       share with a user
//	nimbo share list <remote-path>              list shares on a path
//	nimbo share rm <share-id>                   delete a share
func cmdShare(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nimbo share <link|user|list|rm> ...")
	}
	client, _, err := clientForDefault()
	if err != nil {
		return err
	}

	switch args[0] {
	case "link":
		if len(args) < 2 {
			return fmt.Errorf("usage: nimbo share link <remote-path> [password]")
		}
		opt := transport.PublicLinkOptions{}
		if len(args) >= 3 {
			opt.Password = args[2]
		}
		sh, err := client.CreatePublicLink(ctx, strings.Trim(args[1], "/"), opt)
		if err != nil {
			return err
		}
		fmt.Printf("Public link created:\n  %s\n", sh.URL)
		if opt.Password != "" {
			fmt.Println("  (password-protected)")
		}
		return nil

	case "user":
		if len(args) != 3 {
			return fmt.Errorf("usage: nimbo share user <remote-path> <user>")
		}
		sh, err := client.CreateUserShare(ctx, strings.Trim(args[1], "/"), args[2], transport.PermRead|transport.PermUpdate)
		if err != nil {
			return err
		}
		fmt.Printf("Shared %s with %s (share id %s)\n", args[1], args[2], sh.ID)
		return nil

	case "list":
		if len(args) != 2 {
			return fmt.Errorf("usage: nimbo share list <remote-path>")
		}
		shares, err := client.ListShares(ctx, strings.Trim(args[1], "/"))
		if err != nil {
			return err
		}
		if len(shares) == 0 {
			fmt.Println("No shares on that path.")
			return nil
		}
		for _, s := range shares {
			fmt.Printf("• id %s  %s", s.ID, shareTypeName(s.ShareType))
			if s.ShareWith != "" {
				fmt.Printf(" → %s", s.ShareWith)
			}
			if s.URL != "" {
				fmt.Printf("  %s", s.URL)
			}
			fmt.Println()
		}
		return nil

	case "rm", "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: nimbo share rm <share-id>")
		}
		if err := client.DeleteShare(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("Deleted share %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown share subcommand %q (use link|user|list|rm)", args[0])
	}
}

func shareTypeName(t int) string {
	switch t {
	case transport.ShareTypeUser:
		return "user"
	case transport.ShareTypeGroup:
		return "group"
	case transport.ShareTypePublic:
		return "public-link"
	case transport.ShareTypeEmail:
		return "email"
	default:
		return fmt.Sprintf("type-%d", t)
	}
}
