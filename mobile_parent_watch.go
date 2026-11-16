package main

import (
	"io"
	"os"
)

// The mobile shell apps (mobile/android, mobile/ios) run NextTrace as a helper
// process and keep its stdin open. When the shell exits or is killed, stdin
// reaches EOF and the helper exits with it instead of lingering as an orphan
// that still holds the listen port.
func init() {
	if os.Getenv("NEXTTRACE_EXIT_ON_STDIN_CLOSE") != "1" {
		return
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
}
