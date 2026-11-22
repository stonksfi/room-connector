package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/config"
	"github.com/stonksfi/room-connector/internal/localapi"
	"github.com/stonksfi/room-connector/internal/model/room/entities"
	"github.com/stonksfi/room-connector/internal/store"
	"github.com/stonksfi/room-connector/internal/uplink"
)

const pollEvery = 2 * time.Second

func login(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", cfg.BaseURL, "Room server URL")
	name := fs.String("name", "", "name to show for this machine (default: its hostname)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	deviceName := *name
	if deviceName == "" {
		deviceName = hostname()
	}

	client := uplink.NewClient(*server, func() string { return "" })
	start, err := client.StartPairing(ctx, deviceName, runtime.GOOS)
	if err != nil {
		return fmt.Errorf("start pairing with %s: %w", *server, err)
	}
	fmt.Printf("\n  Link %q to Room\n\n", deviceName)
	fmt.Printf("  In the Room app, open Settings > Link a machine and enter:\n\n")
	fmt.Printf("        %s\n\n", start.GetCode())
	fmt.Printf("  The code expires at %s.\n\n", time.Unix(start.GetExpiresAt(), 0).Format("15:04"))

	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		poll, err := client.PollPairing(ctx, start.GetPairingId(), start.GetPollSecret())
		if err != nil {
			fmt.Fprintf(os.Stderr, "  (waiting: %v)\n", err)
			continue
		}
		switch poll.GetStatus() {
		case entities.PairingStatus_PAIRING_PENDING:
			continue
		case entities.PairingStatus_PAIRING_EXPIRED:
			return errors.New("the code expired before it was entered; run `roomd login` again")
		}
		if err := store.SaveToken(poll.GetDeviceToken()); err != nil {
			return fmt.Errorf("save device token: %w", err)
		}
		cfg.BaseURL = *server
		cfg.DeviceID = poll.GetDevice().GetId()
		cfg.DeviceName = poll.GetDevice().GetName()
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Printf("  Linked to %s. Your sessions on %s now show in Room.\n", poll.GetAccountEmail(), cfg.DeviceName)
		fmt.Println("  If that is not your account, run `roomd logout` now.")
		if !daemonRunning() {
			fmt.Println("  roomd is not running: start it with `roomd install` (or `roomd run`).")
		}
		return nil
	}
}

func logout(ctx context.Context) error {
	if err := store.DeleteToken(); err != nil {
		return err
	}
	tellDaemonLoggedOut(ctx)
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.DeviceID, cfg.DeviceName = 0, ""
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Println("Unlinked this machine. To remove it from your account too, revoke it in the Room app.")
	return nil
}

func printStatus(ctx context.Context) error {
	resp, err := localapi.Client(config.SocketPath(), 2*time.Second).Get("http://roomd/v1/status")
	if err != nil {
		cfg, _ := config.Load()
		_, tokenErr := store.LoadToken()
		fmt.Printf("roomd is not running.\nserver: %s\nlinked: %v\n", cfg.BaseURL, tokenErr == nil)
		return nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	pretty, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println("roomd is running.")
	fmt.Println(string(pretty))
	return nil
}

func daemonRunning() bool {
	resp, err := localapi.Client(config.SocketPath(), time.Second).Get("http://roomd/v1/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "my machine"
	}
	return strings.TrimSuffix(h, ".local")
}
