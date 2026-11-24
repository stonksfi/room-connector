package localapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stonksfi/room-connector/internal/core"
)

type fakeProvider struct{ lastHook, returned, canAnswer, waited string }

func (p *fakeProvider) Name() string                                        { return "fake" }
func (p *fakeProvider) Start(context.Context, core.Sink) error              { return nil }
func (p *fakeProvider) Deliver(context.Context, string, core.Command) error { return nil }
func (p *fakeProvider) HandleHook(_ context.Context, b []byte) ([]byte, error) {
	p.lastHook = string(b)
	return []byte(`{"decision":"block"}`), nil
}
func (p *fakeProvider) TakeInbox(id string) []core.InboxMessage {
	return []core.InboxMessage{{CommandID: 1, Text: "for " + id}}
}
func (p *fakeProvider) ReturnInbox(id string, commandID int64, reason string) {
	p.returned = fmt.Sprintf("%s/%d/%s", id, commandID, reason)
}
func (p *fakeProvider) CanAnswer(id string) { p.canAnswer = id }
func (p *fakeProvider) WaitAnswer(_ context.Context, b []byte) ([]byte, error) {
	if string(b) == "bad" {
		return nil, fmt.Errorf("refused")
	}
	p.waited = string(b)
	return []byte(`{"closed":true}`), nil
}

func TestRoutes(t *testing.T) {
	p := &fakeProvider{}
	srv := httptest.NewServer(New([]core.Provider{p}, func() any { return map[string]int{"queued": 2} }, nil, func() {}).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/providers/fake/hook", "application/json", strings.NewReader(`{"a":1}`))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, `{"decision":"block"}`, string(body))
	assert.Equal(t, `{"a":1}`, p.lastHook)

	resp, err = http.Get(srv.URL + "/v1/providers/fake/inbox?session=s1")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	assert.JSONEq(t, `{"messages":[{"command_id":1,"text":"for s1"}]}`, string(body))

	assert.Empty(t, p.canAnswer, "a script that did not say it takes answers")
	_, err = http.Get(srv.URL + "/v1/providers/fake/inbox?session=s2&caps=other,answer")
	require.NoError(t, err)
	assert.Equal(t, "s2", p.canAnswer)

	resp, err = http.Post(srv.URL+"/v1/providers/fake/answer/wait", "application/json", strings.NewReader(`{"session":"s1"}`))
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	assert.Equal(t, `{"closed":true}`, string(body))
	assert.Equal(t, `{"session":"s1"}`, p.waited)
	resp, err = http.Post(srv.URL+"/v1/providers/fake/answer/wait", "application/json", strings.NewReader(`bad`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, err = http.Get(srv.URL + "/v1/providers/fake/inbox")
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, err = http.Get(srv.URL + "/v1/providers/nope/inbox?session=s1")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, err = http.Post(srv.URL+"/v1/providers/fake/inbox/return", "application/json",
		strings.NewReader(`{"session":"s1","command_id":3,"reason":"dropped"}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "s1/3/dropped", p.returned)

	resp, err = http.Post(srv.URL+"/v1/providers/fake/inbox/return", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, err = http.Get(srv.URL + "/v1/status")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	assert.JSONEq(t, `{"queued":2}`, string(body))
}

func TestListen_SocketIsPrivateAndExclusive(t *testing.T) {
	// A short path: Unix socket paths are limited to about 104 bytes.
	dir, err := os.MkdirTemp("/tmp", "roomd")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")

	ln, err := Listen(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- New(nil, func() any { return "ok" }, nil, func() {}).Serve(ctx, ln) }()

	_, err = Listen(path)
	assert.ErrorIs(t, err, ErrRunning)

	resp, err := Client(path, time.Second).Get("http://roomd/v1/status")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	cancel()
	assert.NoError(t, <-done)
}

type fakeScreen struct{ stops int }

func (s *fakeScreen) Stop() { s.stops++ }
func (s *fakeScreen) Setup(context.Context) (core.ScreenPermissions, error) {
	return core.ScreenPermissions{ScreenRecording: true}, nil
}

func TestScreenRoutes(t *testing.T) {
	screen := &fakeScreen{}
	loggedOut := 0
	srv := httptest.NewServer(New(nil, func() any { return nil }, screen, func() { loggedOut++ }).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/screen/setup", "application/json", nil)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	assert.JSONEq(t, `{"screen_recording":true,"accessibility":false}`, string(body))

	resp, err = http.Post(srv.URL+"/v1/screen/stop", "application/json", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, 1, screen.stops)

	resp, err = http.Post(srv.URL+"/v1/logout", "application/json", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, 1, loggedOut)

	none := httptest.NewServer(New(nil, func() any { return nil }, nil, func() {}).Handler())
	defer none.Close()
	resp, err = http.Post(none.URL+"/v1/screen/setup", "application/json", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}
