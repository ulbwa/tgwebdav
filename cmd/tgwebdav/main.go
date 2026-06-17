// Command tgwebdav runs the WebDAV-over-Telegram server: a single binary that
// serves a per-user WebDAV namespace and an admin Management API over one
// PostgreSQL database, buffering writes in a WAL and packing them into Telegram
// channel blobs.
package main

import "os"

func main() {
	if err := Execute(); err != nil {
		os.Exit(1)
	}
}
