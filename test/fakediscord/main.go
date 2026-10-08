// Command fakediscord is a stand-in Discord webhook for the Compose tests.
// It accepts POST /api/webhooks/{id}/{token} and lists what it received at
// GET /messages.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ivarstudios/syncwatch/test/harness"
)

func main() {
	addr := ":8080"
	if a := os.Getenv("LISTEN"); a != "" {
		addr = a
	}
	fd, err := harness.StartFakeDiscord(addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake Discord webhook listening on %s", addr)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	fd.Close()
}
