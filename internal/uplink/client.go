// Package uplink connects roomd to room-be: RPCs for pairing, sessions,
// events, command acks and screen answers, and the connector stream that
// delivers commands and screen offers.
package uplink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/model/room"
)

// apiVersion is the :version segment of every route.
const apiVersion int32 = 1

// ErrUnauthorized means room-be rejected the device token: the device was
// revoked or never paired.
var ErrUnauthorized = errors.New("room rejected this device's token (revoked?): run `roomd login`")

// HTTPError is an RPC room-be answered with an error.
type HTTPError struct {
	Path    string
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Path, e.Status, e.Message)
}

// IsPermanent reports whether err is a refusal that resending the same
// request cannot fix (a 4xx other than auth, timeout or rate limiting).
func IsPermanent(err error) bool {
	var h *HTTPError
	if !errors.As(err, &h) {
		return false
	}
	switch h.Status {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return h.Status >= 400 && h.Status < 500
}

// Client calls room-be's connector RPCs.
type Client struct {
	baseURL string
	// token returns the device token, read on every call so a daemon started
	// before `roomd login` picks the token up without a restart.
	token func() string
	http  *http.Client
}

// NewClient returns a client; token may return "" before login.
func NewClient(baseURL string, token func() string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// HasToken reports whether the device is logged in.
func (c *Client) HasToken() bool {
	return c.token() != ""
}

// envelope is every RPC response's shape.
type envelope struct {
	Code    int32           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) call(ctx context.Context, path string, req any, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v%d%s", c.baseURL, apiVersion, path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := c.token(); tok != "" {
		httpReq.Header.Set("Authorization", tok)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return &HTTPError{Path: path, Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	if resp.StatusCode != http.StatusOK || env.Code != 0 {
		return &HTTPError{Path: path, Status: resp.StatusCode, Message: env.Message}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func version() *int32 {
	v := apiVersion
	return &v
}

func (c *Client) StartPairing(ctx context.Context, deviceName, platform string) (*room.StartDevicePairingResponseData, error) {
	var out room.StartDevicePairingResponseData
	err := c.call(ctx, "/connector/pairing/start", &room.StartDevicePairingRequest{
		Version: version(),
		Params:  &room.StartDevicePairingRequestParams{DeviceName: &deviceName, Platform: &platform},
	}, &out)
	return &out, err
}

func (c *Client) PollPairing(ctx context.Context, pairingID int64, pollSecret string) (*room.PollDevicePairingResponseData, error) {
	var out room.PollDevicePairingResponseData
	err := c.call(ctx, "/connector/pairing/poll", &room.PollDevicePairingRequest{
		Version: version(),
		Params:  &room.PollDevicePairingRequestParams{PairingId: &pairingID, PollSecret: &pollSecret},
	}, &out)
	return &out, err
}

func (c *Client) SyncSessions(ctx context.Context, params *room.SyncSessionsRequestParams) error {
	return c.call(ctx, "/connector/sessions/sync", &room.SyncSessionsRequest{Version: version(), Params: params}, nil)
}

func (c *Client) PushEvents(ctx context.Context, params *room.PushSessionEventsRequestParams) error {
	return c.call(ctx, "/connector/events", &room.PushSessionEventsRequest{Version: version(), Params: params}, nil)
}

func (c *Client) ListPendingCommands(ctx context.Context) (*room.ListPendingCommandsResponseData, error) {
	var out room.ListPendingCommandsResponseData
	err := c.call(ctx, "/connector/commands", &room.ListPendingCommandsRequest{Version: version()}, &out)
	return &out, err
}

func (c *Client) AckCommand(ctx context.Context, params *room.AckCommandRequestParams) error {
	return c.call(ctx, "/connector/commands/ack", &room.AckCommandRequest{Version: version(), Params: params}, nil)
}

// AnswerScreenView hands room-be the answer to a screen offer, which it
// relays to the phone waiting on it. A 404 means nobody waits any more.
func (c *Client) AnswerScreenView(ctx context.Context, params *room.AnswerScreenViewRequestParams) error {
	return c.call(ctx, "/connector/screen/answer", &room.AnswerScreenViewRequest{Version: version(), Params: params}, nil)
}
