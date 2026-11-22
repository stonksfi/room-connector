// Command roomd-screen serves one view of this Mac's screen to the Room app.
// roomd starts it, one process per view, so that roomd itself never loads
// AppKit, ScreenCaptureKit or VideoToolbox, and a crash here ends one view,
// not session streaming.
//
//	roomd-screen session   answer the offer on stdin, then serve the view (roomd runs this)
//	roomd-screen setup     ask macOS for Screen Recording and Accessibility; print what is granted
//	roomd-screen bench     capture and encode a few seconds of the screen; print fps, bitrate, latency
package main

import (
	"fmt"
	"os"
	"runtime"
)

// The main goroutine stays on the main thread, where the app's event loop
// (the menu bar indicator) must run.
func init() { runtime.LockOSThread() }

const usage = `roomd-screen: the screen view of the Room connector

  roomd-screen session         answer the offer on stdin (roomd runs this)
  roomd-screen setup           ask macOS for the permissions a view needs
  roomd-screen bench [-o F] [-d D]
                               capture and encode the screen; print fps, bitrate, latency
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "session":
		err = runSession()
	case "setup":
		err = runSetup()
	case "bench":
		err = runBench(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "roomd-screen:", err)
		os.Exit(1)
	}
}
