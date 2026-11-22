// Package store keeps roomd's secrets and its offline event queue.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/config"
)

const (
	keychainService = "room-connector"
	keychainAccount = "device-token"
)

// ErrNoToken means the device is not logged in.
var ErrNoToken = errors.New("not logged in: run `roomd login`")

// keychainTimeout bounds a `security` call, which can block on a locked keychain.
const keychainTimeout = 5 * time.Second

// keychainNotFound is `security`'s exit status for a missing item.
const keychainNotFound = 44

// validToken is the shape room-be issues (a prefix and URL-safe base64). The
// token is spliced into a `security -i` command, so nothing else may pass.
var validToken = regexp.MustCompile(`^[A-Za-z0-9_-]{16,256}$`)

// runSecurity runs one `security` command with a timeout and returns its output.
func runSecurity(stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout) // one short, self-contained call
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd.Output()
}

// useKeychain is true on macOS, where the token goes in the login keychain.
// Elsewhere it is a file readable only by its owner.
var useKeychain = runtime.GOOS == "darwin" && os.Getenv("ROOM_TOKEN_FILE") == ""

func tokenFile() string {
	if p := os.Getenv("ROOM_TOKEN_FILE"); p != "" {
		return p
	}
	return filepath.Join(config.Home(), "device-token")
}

// SaveToken stores the device token.
func SaveToken(token string) error {
	if !validToken.MatchString(token) {
		return errors.New("room sent a malformed device token")
	}
	if useKeychain {
		// `security -i` reads the command from stdin, which keeps the token
		// out of the process list. -U updates an existing item.
		_, err := runSecurity(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n",
			keychainService, keychainAccount, token), "-i")
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile()), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tokenFile(), []byte(token), 0o600); err != nil {
		return err
	}
	return os.Chmod(tokenFile(), 0o600) // WriteFile keeps an existing file's mode
}

// LoadToken returns the device token, ErrNoToken when there is none, or
// another error when the store could not be read.
func LoadToken() (string, error) {
	if useKeychain {
		out, err := runSecurity("", "find-generic-password",
			"-s", keychainService, "-a", keychainAccount, "-w")
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == keychainNotFound {
			return "", ErrNoToken
		}
		if err != nil {
			return "", fmt.Errorf("read the keychain: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	raw, err := os.ReadFile(tokenFile())
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoToken
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// DeleteToken forgets the device token.
func DeleteToken() error {
	if useKeychain {
		_, err := runSecurity("", "delete-generic-password",
			"-s", keychainService, "-a", keychainAccount)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == keychainNotFound {
			return nil // nothing stored
		}
		return err
	}
	err := os.Remove(tokenFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
