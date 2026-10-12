// Command fakediscord is a stand-in Discord webhook for the Compose tests.
// It accepts POST /api/webhooks/{id}/{token}, answers a GET of the same URL
// with the webhook object, and lists what it received at GET /messages. POST /mode {"mode": "fail404:3"} (or MODE at the start)
// selects an error answer, with the mode names of the kit's fake Discord:
// ok (the default), fail500[:N], fail400[:N], fail404[:N] and
// ratelimit[:RETRY_AFTER[:N]]. GET /mode shows the current mode.
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
	if m := os.Getenv("MODE"); m != "" {
		if err := fd.SetMode(m); err != nil {
			log.Fatal(err)
		}
	}
	log.Printf("fake Discord webhook listening on %s, mode %s", addr, fd.Mode())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	fd.Close()
}
