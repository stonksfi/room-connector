// Package localapi is roomd's local HTTP API on a Unix socket in ~/.room. The
// agents' hooks and in-session scripts reach their provider through it:
//
//	POST /v1/providers/{name}/hook           a hook's JSON; the reply goes back to the hook
//	GET  /v1/providers/{name}/inbox?session=[&caps=answer]
//	                                         messages queued for an in-session script
//	                                         (caps=answer: it takes answers from Room)
//	POST /v1/providers/{name}/inbox/return   a message the script could not submit
//	POST /v1/providers/{name}/answer/wait    held until Room answers the question the
//	                                         body names (a long poll); the reply goes back verbatim
//	GET  /v1/status                          roomd's health, for `roomd status`
//	POST /v1/screen/setup                    ask macOS for the screen view's permissions (`roomd screen setup`)
//	POST /v1/screen/stop                     end the running screen view (`roomd screen stop`)
//	POST /v1/logout                          the device token was deleted (`roomd logout`)
//
// The socket's directory is the user's own (0700), so only they can reach it.
package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
)

// maxHookBody bounds a hook's JSON; tool inputs can be large, transcripts are not sent.
const maxHookBody = 8 << 20

type Server struct {
	providers map[string]core.Provider
	status    func() any
	screen    Screen // nil where this build cannot show the screen
	logout    func()
}

// Screen is the screen view host as `roomd screen` drives it.
type Screen interface {
	Setup(ctx context.Context) (core.ScreenPermissions, error)
	Stop()
}

// New serves the providers' routes; logout runs when `roomd logout` has
// deleted the device token, so roomd stops using it at once.
func New(providers []core.Provider, status func() any, screen Screen, logout func()) *Server {
	byName := make(map[string]core.Provider, len(providers))
	for _, p := range providers {
		byName[p.Name()] = p
	}
	return &Server{providers: byName, status: status, screen: screen, logout: logout}
}

// Handler routes the local API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/providers/{name}/hook", s.hook)
	mux.HandleFunc("GET /v1/providers/{name}/inbox", s.inbox)
	mux.HandleFunc("POST /v1/providers/{name}/inbox/return", s.inboxReturn)
	mux.HandleFunc("POST /v1/providers/{name}/answer/wait", s.answerWait)
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.status())
	})
	mux.HandleFunc("POST /v1/screen/setup", s.screenSetup)
	mux.HandleFunc("POST /v1/screen/stop", s.screenStop)
	mux.HandleFunc("POST /v1/logout", func(w http.ResponseWriter, r *http.Request) {
		s.logout()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// errNoScreen answers the screen routes on a build that cannot show the screen.
const errNoScreen = "this machine cannot show its screen in Room (macOS only)"

// screenSetup runs in roomd, not the CLI, so that macOS attributes the
// permission requests to roomd.
func (s *Server) screenSetup(w http.ResponseWriter, r *http.Request) {
	if s.screen == nil {
		http.Error(w, errNoScreen, http.StatusNotImplemented)
		return
	}
	perms, err := s.screen.Setup(r.Context())
	if err != nil {
		log.Printf("screen setup: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, perms)
}

func (s *Server) screenStop(w http.ResponseWriter, r *http.Request) {
	if s.screen == nil {
		http.Error(w, errNoScreen, http.StatusNotImplemented)
		return
	}
	s.screen.Stop()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	recv, ok := s.providers[r.PathValue("name")].(core.HookReceiver)
	if !ok {
		http.NotFound(w, r)
		return
	}
	relay(w, r, "hook", recv.HandleHook)
}

func (s *Server) answerWait(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.providers[r.PathValue("name")].(core.AnswerServer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	relay(w, r, "answer wait", srv.WaitAnswer)
}

// relay hands a request's body (a hook's JSON: tool inputs, plans included,
// can be large) to the provider and its reply back verbatim.
func relay(w http.ResponseWriter, r *http.Request, what string, handle func(context.Context, []byte) ([]byte, error)) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reply, err := handle(r.Context(), body)
	if err != nil {
		log.Printf("%s %s: %v", r.PathValue("name"), what, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(reply); err != nil {
		log.Printf("%s %s: write reply: %v", r.PathValue("name"), what, err)
	}
}

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.providers[r.PathValue("name")].(core.InboxServer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	session := r.URL.Query().Get("session")
	if session == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return
	}
	if ans, ok := srv.(core.AnswerServer); ok && slices.Contains(strings.Split(r.URL.Query().Get("caps"), ","), "answer") {
		ans.CanAnswer(session)
	}
	writeJSON(w, map[string]any{"messages": srv.TakeInbox(session)})
}

// returned is what an in-session script sends back for a message it took
// but could not submit.
type returned struct {
	Session   string `json:"session"`
	CommandID int64  `json:"command_id"`
	Reason    string `json:"reason"`
}

func (s *Server) inboxReturn(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.providers[r.PathValue("name")].(core.InboxServer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var in returned
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil || in.Session == "" || in.CommandID == 0 {
		http.Error(w, "session and command_id are required", http.StatusBadRequest)
		return
	}
	srv.ReturnInbox(in.Session, in.CommandID, in.Reason)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

// ErrRunning means another roomd already serves the socket.
var ErrRunning = errors.New("roomd is already running")

// Listen opens the socket, replacing a stale one left by a crashed roomd.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return nil, ErrRunning
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve runs the API on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// BaseContext: requests end with roomd, so a held answer wait never
	// outlasts a shutdown.
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		// ctx is done: shutting down needs a fresh, short deadline of its own.
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Printf("local API shutdown: %v", err)
		}
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Client returns an HTTP client that talks to the socket; URLs use host "roomd".
func Client(path string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}
}
