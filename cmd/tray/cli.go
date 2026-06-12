//go:build !tray

// Without the `tray` build tag, cmd/tray builds as a dependency-free status CLI
// that prints the same information the menu-bar app would show. This keeps the
// command buildable and testable in headless environments; build with
// `-tags tray` for the native menu-bar app.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/uwin/cnc-proxy/internal/apiclient"
)

var (
	apiBase   = flag.String("api", "http://127.0.0.1:8420", "proxy API base URL")
	authUser  = flag.String("auth-user", envDefault("CNC_AUTH_USER", "cnc"), "HTTP Basic Auth username")
	authToken = flag.String("auth-token", envDefault("CNC_AUTH_TOKEN", ""), "HTTP Basic Auth token/password")
	watch     = flag.Bool("watch", false, "poll and print status continuously")
	poll      = flag.Duration("poll", 3*time.Second, "poll interval when watching")
)

func main() {
	flag.Parse()
	client := apiclient.NewWithAuth(*apiBase, *authUser, *authToken)

	if !*watch {
		if err := printStatus(client); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	for {
		if err := printStatus(client); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		time.Sleep(*poll)
	}
}

func printStatus(client *apiclient.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	st, err := client.Machine(ctx)
	if err != nil {
		return err
	}
	files, err := client.Files(ctx)
	if err != nil {
		return err
	}
	pending := apiclient.PendingCount(files)
	sync := "up to date"
	if pending > 0 {
		sync = fmt.Sprintf("%d pending", pending)
	}
	fmt.Printf("machine=%s mode=%s sync=%s\n", stateOrUnknown(st.State), st.Mode, sync)
	return nil
}

func stateOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func envDefault(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}
