//go:build darwin && cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/stonksfi/room-connector/internal/screen/macos"
	"github.com/stonksfi/room-connector/internal/screen/session"
)

func init() { log.SetFlags(0) } // roomd's log stamps each line

func runSession() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	// stdout carries the one reply line; anything else printed goes to the log.
	reply := os.Stdout
	os.Stdout = os.Stderr
	p := session.Platform{
		Source:      macos.NewCapture(),
		Injector:    macos.Injector{},
		Indicator:   macos.Indicator{},
		Permissions: macos.Permissions,
	}
	var reason session.Reason
	var err error
	macos.Run(func() { reason, err = session.Serve(ctx, os.Stdin, reply, p) })
	if reason != "" {
		log.Printf("view ended: %s", reason)
	}
	return err
}

func runSetup() error {
	return json.NewEncoder(os.Stdout).Encode(macos.RequestPermissions())
}

func runBench(args []string) error {
	flags := flag.NewFlagSet("bench", flag.ContinueOnError)
	out := flags.String("o", filepath.Join(os.TempDir(), "roomd-screen-bench.h264"), "file to write the H.264 stream to")
	d := flags.Duration("d", 5*time.Second, "how long to capture")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !macos.Permissions().ScreenRecording {
		return errors.New("Screen Recording is not allowed for this process: allow the app that ran it " +
			"(roomd, or your terminal) in System Settings > Privacy & Security > Screen & System Audio Recording")
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	var stats *benchStats
	macos.Run(func() { stats, err = bench(f, *d) })
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	stats.print(*out, *d)
	return nil
}

type benchStats struct {
	mu        sync.Mutex
	frames    int
	keyframes int
	bytes     int
	encode    []time.Duration
	writeErr  error
}

func (s *benchStats) add(w io.Writer, f session.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames++
	s.bytes += len(f.Data)
	s.encode = append(s.encode, f.Encode)
	if f.Keyframe {
		s.keyframes++
	}
	if _, err := w.Write(f.Data); err != nil && s.writeErr == nil {
		s.writeErr = err
	}
}

// bench captures for d, asking for a keyframe halfway to check that one
// comes even from a still screen.
func bench(w io.Writer, d time.Duration) (*benchStats, error) {
	s := &benchStats{}
	src := macos.NewCapture()
	failed := make(chan error, 1)
	err := src.Start(func(f session.Frame) { s.add(w, f) }, func(err error) {
		select {
		case failed <- err:
		default:
		}
	})
	if err != nil {
		return nil, fmt.Errorf("start capture: %w", err)
	}
	select {
	case <-time.After(d / 2):
		src.ForceKeyframe()
		select {
		case <-time.After(d - d/2):
		case err = <-failed:
		}
	case err = <-failed:
	}
	src.Stop()
	if err != nil {
		return nil, fmt.Errorf("capture stopped: %w", err)
	}
	return s, s.writeErr
}

func (s *benchStats) print(path string, d time.Duration) {
	w, h := macos.CaptureSize()
	fmt.Printf("captured %dx%d for %s into %s\n", w, h, d, path)
	fmt.Printf("  frames     %d (%.1f fps; frames flow only while the screen changes)\n", s.frames, float64(s.frames)/d.Seconds())
	fmt.Printf("  keyframes  %d\n", s.keyframes)
	fmt.Printf("  bitrate    %.2f Mbps\n", float64(s.bytes*8)/d.Seconds()/1e6)
	if len(s.encode) > 0 {
		slices.Sort(s.encode)
		at := func(q float64) time.Duration { return s.encode[int(q*float64(len(s.encode)-1))] }
		fmt.Printf("  encode     p50 %s  p95 %s\n", at(0.5).Round(10*time.Microsecond), at(0.95).Round(10*time.Microsecond))
	}
	fmt.Printf("check it plays: ffplay -f h264 %s\n", path)
}
