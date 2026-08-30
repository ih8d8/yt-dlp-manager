// Command yt-dlp-manager is the single application binary: TUI, headless
// daemon, HTTP server with embedded web UI, and scriptable CLI.
package main

import (
	"os"

	"yt-dlp-manager/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
