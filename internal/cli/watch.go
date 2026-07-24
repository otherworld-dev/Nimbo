package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/transfer"
)

// cmdWatch continuously keeps a local folder in sync with a remote folder and
// surfaces app notifications as desktop toasts, until Ctrl-C. With notify_push
// it's real-time; otherwise it polls.
//
//	nimbo watch <local-dir> [remote-dir]
func cmdWatch(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: nimbo watch <local-dir> [remote-dir]")
	}
	p := agent.Pair{LocalDir: args[0]}
	if len(args) == 2 {
		p.RemoteRoot = strings.Trim(args[1], "/")
	}

	eng, err := agent.NewEngine(ctx)
	if err != nil {
		return err
	}
	mode := "polling every 15s"
	if eng.PushAvailable() {
		mode = "real-time (notify_push)"
	}
	fmt.Printf("Watching %s  <->  %s/%s\nMode: %s. Ctrl-C to stop.\n\n",
		p.LocalDir, eng.Account.ServerURL, p.RemoteRoot, mode)

	return eng.Run(ctx, []agent.Pair{p}, func(_ agent.Pair, stats transfer.Stats) {
		printStats(stats) // no-ops when nothing changed
	})
}
