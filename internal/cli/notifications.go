package cli

import (
	"context"
	"fmt"
	"strings"
)

// cmdNotifications lists the account's current Nextcloud notifications.
//
//	nimbo notifications
func cmdNotifications(ctx context.Context, _ []string) error {
	client, _, err := clientForDefault()
	if err != nil {
		return err
	}
	items, err := client.Notifications(ctx)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("No notifications.")
		return nil
	}
	for _, it := range items {
		fmt.Printf("• [%s] %s\n", it.App, it.Subject)
		if msg := strings.TrimSpace(it.Message); msg != "" {
			fmt.Printf("    %s\n", msg)
		}
	}
	return nil
}
