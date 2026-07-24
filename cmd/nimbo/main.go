// Command nimbo is the developer/control CLI for Nimbo. In this
// phase it proves the end-to-end transport: interactive login (Login Flow v2),
// capabilities discovery, and basic WebDAV operations (ls/get/put).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/otherworld/nimbo/internal/cli"
)

func main() {
	// Structured, level-controlled logging to stderr; data output goes to stdout.
	level := slog.LevelInfo
	if os.Getenv("NEXTCLIENT_DEBUG") != "" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := cli.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
