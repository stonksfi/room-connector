// Package daemon wires roomd together: the providers, the uplink to room-be,
// the local API their hooks call, and the screen host.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/stonksfi/room-connector/internal/config"
	"github.com/stonksfi/room-connector/internal/core"
	"github.com/stonksfi/room-connector/internal/localapi"
	"github.com/stonksfi/room-connector/internal/provider/claudecode"
	"github.com/stonksfi/room-connector/internal/screen"
	"github.com/stonksfi/room-connector/internal/store"
	"github.com/stonksfi/room-connector/internal/uplink"
)

// Providers lists the agents this build of roomd supports. Adding an agent
// is one more entry here and its package under internal/provider.
func Providers(cfg config.Config) []core.Provider {
	return []core.Provider{
		claudecode.New(cfg.ProviderSetting(claudecode.Name, "bin")),
	}
}

// screenHost is the screen view host, roomd-screen beside roomd, or nil where
// this build cannot show the screen.
func screenHost() *screen.Host {
	if !screen.Supported {
		return nil
	}
	path, err := screen.Beside()
	if err != nil {
		log.Printf("screen view off: find roomd-screen: %v", err)
		return nil
	}
	return screen.NewHost(path)
}

// status is what GET /v1/status answers.
type status struct {
	uplink.Status
	BaseURL    string `json:"base_url"`
	DeviceName string `json:"device_name,omitempty"`
	LoggedIn   bool   `json:"logged_in"`
}

// Run serves until ctx ends, or until the local API fails.
func Run(ctx context.Context, cfg config.Config) error {
	ln, err := localapi.Listen(config.SocketPath())
	if err != nil {
		return err
	}
	queue, err := store.OpenQueue(filepath.Join(config.Home(), "queue.jsonl"))
	if err != nil {
		return fmt.Errorf("open event queue: %w", err)
	}
	defer queue.Close()
	commands, err := store.OpenCommandLog(filepath.Join(config.Home(), "commands.json"))
	if err != nil {
		return fmt.Errorf("open command log: %w", err)
	}

	// Without its socket roomd is useless: stop everything so launchd restarts it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tokens := &tokenCache{load: store.LoadToken, now: time.Now}
	providers := Providers(cfg)
	// Nil interfaces turn screen views off: a nil *screen.Host must not be
	// handed over as a non-nil interface.
	var screens core.ScreenHost
	var screenAPI localapi.Screen
	host := screenHost()
	if host != nil {
		defer host.Stop() // views end with ctx too; this waits for the last to exit
		screens, screenAPI = host, host
	}
	logout := func() {
		tokens.forget()
		if host != nil {
			host.Stop() // a view never outlives the link it came through
		}
	}
	up := uplink.New(uplink.NewClient(cfg.BaseURL, tokens.get), queue, commands, providers, screens)
	api := localapi.New(providers, func() any {
		return status{Status: up.Status(), BaseURL: cfg.BaseURL, DeviceName: cfg.DeviceName, LoggedIn: tokens.get() != ""}
	}, screenAPI, logout)

	var wg sync.WaitGroup
	for _, p := range providers {
		wg.Add(1)
		go func(p core.Provider) {
			defer wg.Done()
			if err := p.Start(ctx, up); err != nil {
				log.Printf("provider %s stopped: %v", p.Name(), err)
			}
		}(p)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		up.Run(ctx)
	}()
	log.Printf("roomd serving %s, uploading to %s", config.SocketPath(), cfg.BaseURL)
	err = api.Serve(ctx, ln)
	cancel()
	wg.Wait()
	return err
}

const (
	tokenFresh = time.Minute     // how long a loaded token is reused
	tokenRetry = 5 * time.Second // how soon a missing token is looked for again
)

// tokenCache reads the device token from the credential store now and then,
// so a login while roomd runs is picked up within seconds and the keychain is
// not queried on every request. A failed read keeps the last good token.
type tokenCache struct {
	load func() (string, error)
	now  func() time.Time

	mu      sync.Mutex
	token   string
	at      time.Time
	lastErr string
}

// forget drops the token at once, after `roomd logout` deleted it, instead
// of using it for up to tokenFresh more.
func (c *tokenCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.at = "", c.now()
}

func (c *tokenCache) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	age := c.now().Sub(c.at)
	if !c.at.IsZero() && (age < tokenRetry || (c.token != "" && age < tokenFresh)) {
		return c.token
	}
	tok, err := c.load()
	c.at = c.now()
	switch {
	case err == nil:
		c.token, c.lastErr = tok, ""
	case errors.Is(err, store.ErrNoToken):
		c.token, c.lastErr = "", ""
	default:
		if err.Error() != c.lastErr {
			log.Printf("read device token (keeping the last one): %v", err)
		}
		c.lastErr = err.Error()
	}
	return c.token
}
