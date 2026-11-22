package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/config"
	"github.com/stonksfi/room-connector/internal/screen"
	"github.com/stonksfi/room-connector/internal/store"
	"github.com/stonksfi/room-connector/plugins"
)

const (
	launchdLabel    = "com.stonksfi.roomd"
	marketplaceName = "room-local"
	pluginName      = "room"
	// signIdentityEnv names a code-signing identity to sign roomd and
	// roomd-screen with, so macOS keeps their Screen Recording and
	// Accessibility grants across rebuilds. Ad-hoc signatures change with
	// every build, and macOS then forgets the grants.
	signIdentityEnv = "ROOM_CODESIGN_IDENTITY"
)

func binPath() string        { return filepath.Join(config.Home(), "bin", "roomd") }
func screenBinPath() string  { return filepath.Join(config.Home(), "bin", "roomd-screen") }
func marketplaceDir() string { return filepath.Join(config.Home(), "claude-plugin") }
func launchdPlistPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", launchdLabel+".plist")
}

// install puts roomd and the Claude Code plugin in place and starts the
// daemon. It is safe to run again, e.g. after upgrading roomd.
func install(ctx context.Context) error {
	if err := installBinaries(ctx); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	fmt.Println("✓ roomd installed at", binPath())

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude is not on PATH: install Claude Code first")
	}
	cfg.SetProviderSetting("claude_code", "bin", claude)
	if err := config.Save(cfg); err != nil {
		return err
	}

	if err := installPlugin(ctx, claude); err != nil {
		fmt.Println("! could not install the Claude Code plugin automatically:", err)
		fmt.Printf("  install it yourself:\n    claude plugin marketplace add %s\n    claude plugin install %s@%s --scope user\n",
			marketplaceDir(), pluginName, marketplaceName)
	} else {
		fmt.Println("✓ Claude Code plugin installed (sessions already open: run /reload-plugins in them)")
	}

	if runtime.GOOS != "darwin" {
		fmt.Println("! not macOS: run `roomd run` under your service manager to keep it running")
	} else if err := installLaunchd(ctx); err != nil {
		return fmt.Errorf("start roomd with launchd: %w", err)
	} else {
		fmt.Println("✓ roomd runs in the background and starts at login")
	}

	if _, err := store.LoadToken(); err != nil {
		fmt.Println("\nNext: run `roomd login` to link this machine to your Room account.")
	}
	return nil
}

// installBinaries installs roomd and, on macOS, roomd-screen from beside it,
// signing both when signIdentityEnv names an identity. It never asks macOS
// for a permission: granting them at the Mac is the owner's own step.
func installBinaries(ctx context.Context) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	identity := os.Getenv(signIdentityEnv)
	if err := installFile(ctx, self, binPath(), "com.stonksfi.roomd", identity); err != nil {
		return err
	}
	if !screen.Supported {
		return nil
	}
	screenSrc, err := screen.Beside()
	if err != nil {
		return err
	}
	if _, err := os.Stat(screenSrc); err != nil {
		fmt.Println("! roomd-screen is not next to roomd, so this screen cannot be viewed from Room (`make build` builds both)")
		return nil
	}
	if err := installFile(ctx, screenSrc, screenBinPath(), "com.stonksfi.roomd-screen", identity); err != nil {
		return fmt.Errorf("roomd-screen: %w", err)
	}
	if identity != "" {
		fmt.Printf("✓ roomd and roomd-screen signed by %q: screen permissions survive reinstalls\n", identity)
	} else {
		fmt.Printf("! roomd is ad-hoc signed, so macOS forgets its screen permissions on every install:\n"+
			"  run `roomd screen setup` again after installing (or set %s to a signing identity)\n", signIdentityEnv)
	}
	return nil
}

// installFile copies src to dst through a file beside it, signed with
// identity if one is given, then renamed into place, so a running copy keeps
// its file and dst is never half-written or unsigned.
func installFile(ctx context.Context, src, dst, signingID, identity string) error {
	if src == dst {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if identity != "" {
		if err := run(exec.CommandContext(ctx, "codesign", "-f", "-s", identity, "--identifier", signingID, tmp)); err != nil {
			return fmt.Errorf("codesign: %w", err)
		}
	}
	return os.Rename(tmp, dst)
}

// run runs cmd, folding its output into the error when it fails.
func run(cmd *exec.Cmd) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeMarketplace writes the embedded plugin into a local marketplace folder.
func writeMarketplace(dir string) error {
	pluginDir := filepath.Join(dir, "plugins", pluginName)
	if err := os.RemoveAll(pluginDir); err != nil {
		return err
	}
	err := fs.WalkDir(plugins.ClaudeCode, "claude-code", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(pluginDir, strings.TrimPrefix(path, "claude-code"))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := plugins.ClaudeCode.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(path, "claude-code/bin/") {
			mode = 0o755
		}
		return os.WriteFile(target, raw, mode)
	})
	if err != nil {
		return err
	}
	manifest := map[string]any{
		"name":  marketplaceName,
		"owner": map[string]string{"name": "Room"},
		"plugins": []map[string]string{{
			"name":        pluginName,
			"source":      "./plugins/" + pluginName,
			"description": "Streams Claude Code sessions to the Room app and takes messages from it.",
		}},
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"), raw, 0o644)
}

// installPlugin writes the embedded plugin into a local marketplace folder
// and installs it from there. Claude Code reads a folder marketplace's
// plugin from the folder itself, so a reinstall updates it in place.
func installPlugin(ctx context.Context, claude string) error {
	dir := marketplaceDir()
	if err := writeMarketplace(dir); err != nil {
		return err
	}
	// Adding a marketplace that is already added fails harmlessly; the
	// install below reports any real problem.
	_ = exec.CommandContext(ctx, claude, "plugin", "marketplace", "add", dir).Run()
	return run(exec.CommandContext(ctx, claude, "plugin", "install", pluginName+"@"+marketplaceName, "--scope", "user"))
}

func launchdTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func launchdService() string {
	return launchdTarget() + "/" + launchdLabel
}

func installLaunchd(ctx context.Context) error {
	logPath := filepath.Join(config.Home(), "roomd.log")
	// ProcessType Standard, not Background: Background throttles roomd and its
	// children so hard that roomd-screen, which loads AppKit, ScreenCaptureKit
	// and VideoToolbox, took over 10s to start (measured: 17s under launchd
	// Background, 0.07s under Standard), so every screen view timed out.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Standard</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, binPath(), logPath, logPath)
	if err := os.MkdirAll(filepath.Dir(launchdPlistPath()), 0o755); err != nil {
		return err
	}
	// A missing or unreadable old plist only means the job is (re)loaded below.
	old, _ := os.ReadFile(launchdPlistPath())
	if err := os.WriteFile(launchdPlistPath(), []byte(plist), 0o644); err != nil {
		return err
	}
	service := launchdService()
	loaded := func() bool {
		return exec.CommandContext(ctx, "launchctl", "print", service).Run() == nil
	}
	if loaded() {
		if string(old) == plist {
			// Same job, new binary: launchd restarts the process in place.
			return run(exec.CommandContext(ctx, "launchctl", "kickstart", "-k", service))
		}
		// The job itself changed, so reload it. bootout returns before launchd
		// has torn the old job down (roomd takes up to a few seconds to stop),
		// and bootstrapping over it fails with "Bootstrap failed: 5: Input/output
		// error". A failed bootout shows up as the wait running out.
		_ = exec.CommandContext(ctx, "launchctl", "bootout", service).Run()
		if !waitUnloaded(ctx, loaded, 10*time.Second, 100*time.Millisecond) {
			return errors.New("the previous roomd is still stopping; run `roomd install` again in a few seconds")
		}
	}
	return run(exec.CommandContext(ctx, "launchctl", "bootstrap", launchdTarget(), launchdPlistPath()))
}

// waitUnloaded polls loaded until it reports false, and says whether that
// happened before limit passed or ctx ended.
func waitUnloaded(ctx context.Context, loaded func() bool, limit, every time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for loaded() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(every):
		}
	}
	return true
}

func uninstall(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := flags.Bool("purge", false, "also delete ~/.room and the device token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var failed []string
	step := func(name string, err error) {
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if runtime.GOOS == "darwin" {
		// bootout fails when roomd is not loaded, which is the goal anyway.
		_ = exec.CommandContext(ctx, "launchctl", "bootout", launchdService()).Run()
		if err := os.Remove(launchdPlistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			step("remove the launchd agent", err)
		}
	}
	if claude, err := exec.LookPath("claude"); err != nil {
		step("remove the Claude Code plugin", errors.New("claude is not on PATH; run `claude plugin uninstall room@room-local`"))
	} else {
		step("remove the Claude Code plugin", exec.CommandContext(ctx, claude, "plugin", "uninstall", pluginName+"@"+marketplaceName).Run())
		step("remove the plugin marketplace", exec.CommandContext(ctx, claude, "plugin", "marketplace", "remove", marketplaceName).Run())
	}
	if len(failed) > 0 {
		return fmt.Errorf("uninstall was incomplete:\n  %s", strings.Join(failed, "\n  "))
	}
	fmt.Println("Removed the roomd daemon and the Claude Code plugin.")
	if *purge {
		if err := store.DeleteToken(); err != nil {
			return err
		}
		if err := os.RemoveAll(config.Home()); err != nil {
			return err
		}
		fmt.Println("Deleted", config.Home(), "and the device token.")
	}
	return nil
}
