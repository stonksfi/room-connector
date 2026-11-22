package uplink

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// streamIdle ends a stream that has gone quiet: room-be pings every 25s, so
// silence this long means the connection died (often across sleep and wake).
const streamIdle = 60 * time.Second

// Stream holds the connector stream open and calls onEvent for each event
// until the stream ends, ctx ends, goes quiet, or the read fails. The caller
// reconnects.
func (c *Client) Stream(ctx context.Context, onEvent func(event string, data []byte)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(streamIdle, cancel)
	defer idle.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v%d/stream/connector", c.baseURL, apiVersion), strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", c.token())
	// No client timeout: the stream is long-lived and ends with ctx.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("connector stream: HTTP %d", resp.StatusCode)
	}
	err = readEvents(resp.Body, func() { idle.Reset(streamIdle) }, onEvent)
	if ctx.Err() != nil && err != nil {
		return fmt.Errorf("connector stream went quiet: %w", err)
	}
	return err
}

// readEvents parses a text/event-stream body: "event:" and "data:" lines,
// each event ended by a blank line.
// onLine is called for every line read, events and pings alike.
func readEvents(body interface{ Read([]byte) (int, error) }, onLine func(), onEvent func(event string, data []byte)) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var event string
	var data strings.Builder
	for sc.Scan() {
		onLine()
		line := sc.Text()
		switch {
		case line == "":
			if event != "" || data.Len() > 0 {
				onEvent(event, []byte(data.String()))
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return sc.Err()
}
