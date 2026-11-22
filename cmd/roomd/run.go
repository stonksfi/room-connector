package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/config"
	"github.com/stonksfi/room-connector/internal/daemon"
	"github.com/stonksfi/room-connector/internal/localapi"
)

func runDaemon(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return daemon.Run(ctx, cfg)
}

// emit forwards a hook's stdin to roomd and prints roomd's reply for the hook
// to read. Any failure is silent: hooks fail open.
func emit(args []string) {
	// Always read stdin to the end, so the hook never sees a broken pipe.
	defer func() { _, _ = io.Copy(io.Discard, os.Stdin) }() // best effort: nothing to report to
	if len(args) != 1 {
		return
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		return
	}
	client := localapi.Client(config.SocketPath(), 3*time.Second)
	resp, err := client.Post("http://roomd/v1/providers/"+args[0]+"/hook", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(os.Stdout, resp.Body) // a hook that went away cannot be told
	}
}
