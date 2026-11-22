package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/stonksfi/room-connector/internal/screen"
)

// maxRequest bounds the request line: an offer of at most 64 KiB plus its ICE
// servers, JSON-escaped.
const maxRequest = 256 << 10

// Serve is `roomd-screen session`: it reads one screen.Request from in,
// writes one screen.Reply to out, then serves the view until it ends. EOF on
// in after the request means roomd is gone, which ends the view.
func Serve(ctx context.Context, in io.Reader, out io.Writer, p Platform) (Reason, error) {
	return serve(ctx, in, out, p, defaults)
}

func serve(ctx context.Context, in io.Reader, out io.Writer, p Platform, opts options) (Reason, error) {
	fail := func(err error) (Reason, error) {
		return "", errors.Join(err, reply(out, screen.Reply{Unavailable: screen.Failed}))
	}
	r := bufio.NewReaderSize(in, maxRequest)
	line, err := r.ReadSlice('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return fail(fmt.Errorf("read the request: %w", err))
	}
	var req screen.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return fail(fmt.Errorf("decode the request: %w", err))
	}
	switch perms := p.Permissions(); {
	case !perms.ScreenRecording:
		return "", reply(out, screen.Reply{Unavailable: screen.NeedsScreenRecording})
	case !perms.Accessibility:
		return "", reply(out, screen.Reply{Unavailable: screen.NeedsAccessibility})
	}
	v, sdp, err := answer(ctx, req.Offer, p, opts)
	if err != nil {
		return fail(fmt.Errorf("answer %s: %w", req.Offer, err))
	}
	if err := reply(out, screen.Reply{SDP: sdp}); err != nil {
		v.end(Lost) // nobody will hear of this view
		return Lost, err
	}
	go func() {
		// The request is all roomd sends; its pipe closing means roomd is gone.
		if _, err := io.Copy(io.Discard, r); err != nil {
			log.Printf("read stdin: %v", err)
		}
		v.Stop()
	}()
	return v.Run(ctx), nil
}

func reply(out io.Writer, r screen.Reply) error {
	if err := json.NewEncoder(out).Encode(r); err != nil {
		return fmt.Errorf("write the reply: %w", err)
	}
	return nil
}
