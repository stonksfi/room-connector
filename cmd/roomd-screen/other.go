//go:build !(darwin && cgo)

package main

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/stonksfi/room-connector/internal/screen"
)

var errNotMac = errors.New("roomd-screen runs only on macOS, built with cgo")

func runSession() error {
	// Answer roomd, so it is not left waiting.
	if err := json.NewEncoder(os.Stdout).Encode(screen.Reply{Unavailable: screen.Failed}); err != nil {
		return errors.Join(errNotMac, err)
	}
	return errNotMac
}

func runSetup() error           { return errNotMac }
func runBench(_ []string) error { return errNotMac }
