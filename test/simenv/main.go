// Command simenv starts the simulated deployment (3 servers, 2 PCs, a fake
// Discord webhook and syncwatch) and keeps it running for manual testing.
//
//	go run ./test/simenv -syncthing /path/to/syncthing -dir /tmp/swsim
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ivarstudios/syncwatch/test/harness"
)

func main() {
	dir := flag.String("dir", filepath.Join(os.TempDir(), "swsim"), "working directory (wiped)")
	bin := flag.String("syncthing", "syncthing", "Syncthing binary")
	sw := flag.String("syncwatch", "", "syncwatch binary (default: build it)")
	port := flag.Int("port", 18400, "first Syncthing GUI port")
	listen := flag.String("listen", "127.0.0.1:18080", "syncwatch listen address")
	discordAddr := flag.String("discord", "127.0.0.1:18099", "fake Discord listen address")
	duration := flag.Duration("for", 0, "stop after this long (0 = until Ctrl-C)")
	demo := flag.Bool("demo", false, "create a mix of problems after start (paused folder, nested folder, stopped folder, offline PC)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	logf := func(f string, a ...any) { log.Printf(f, a...) }

	env, err := harness.Start(ctx, harness.DefaultSpec(filepath.Join(*dir, "st"), *bin, *port), logf)
	if err != nil {
		log.Fatal(err)
	}
	defer env.Close()
	if err := env.BuildIVAR(ctx); err != nil {
		log.Fatal(err)
	}
	if err := env.WaitConnected(ctx, 60*time.Second); err != nil {
		log.Printf("warning: %v", err)
	}
	fd, err := harness.StartFakeDiscord(*discordAddr)
	if err != nil {
		log.Fatal(err)
	}
	defer fd.Close()

	if *sw == "" {
		exe := "syncwatch"
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		*sw = filepath.Join(*dir, exe)
		out, err := exec.Command("go", "build", "-o", *sw, "./cmd/syncwatch").CombinedOutput()
		if err != nil {
			log.Fatalf("building syncwatch: %v\n%s", err, out)
		}
	}
	data := filepath.Join(*dir, "data")
	_ = os.MkdirAll(data, 0o755)
	token := "swt_simulation_token_0123456789"
	if err := harness.WriteConfig(filepath.Join(data, "syncwatch.yaml"), env, fd.URL(), "424242", token, ""); err != nil {
		log.Fatal(err)
	}
	s, err := harness.StartSyncwatch(*sw, data, *listen, "simulation-admin", token)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Stop()

	if *demo {
		go makeDemoProblems(ctx, env)
	}
	fmt.Printf("\nsyncwatch:     %s  (admin password: simulation-admin)\n", s.URL())
	fmt.Printf("status API:    curl -H 'Authorization: Bearer %s' %s/api/v1/status\n", token, s.URL())
	fmt.Printf("fake Discord:  http://%s/messages\n", *discordAddr)
	fmt.Printf("syncwatch log: %s\n", s.LogPath())
	for _, inst := range env.Order {
		fmt.Printf("%-8s %-6s %s  key %s\n", inst.Name, map[bool]string{true: "server", false: "PC"}[inst.Server], inst.URL(), inst.APIKey)
	}
	if *duration > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(*duration):
		}
	} else {
		<-ctx.Done()
	}
}

// makeDemoProblems creates a few problems once syncwatch has settled.
func makeDemoProblems(ctx context.Context, env *harness.Env) {
	time.Sleep(20 * time.Second)
	steps := []struct {
		what string
		fn   func() error
	}{
		{"pause LIVE-GAMMA on SIRIUS", func() error { return env.Get("SIRIUS").PauseFolder(ctx, "p-gamma", true) }},
		{"nested project folder in LIVE-ALPHA", func() error {
			_, err := env.Mkdir("AKKA", "AKKA-LIVE-2PROJECTFILES", "LIVE-ALPHA-2PROJECTFILES", "renders", "LIVE-ALPHA-OLD-2PROJECTFILES")
			_ = env.Get("AKKA").Rescan(ctx, "p-alpha")
			return err
		}},
		{"stop ME-LIVE-DELTA on SOL (marker removed)", func() error {
			err := os.RemoveAll(filepath.Join(env.Get("SOL").ShareDir("SOL-ME-LIVE-1SOURCE"), "ME-LIVE-DELTA-1SOURCE", ".stfolder"))
			_ = env.Get("SOL").Rescan(ctx, "p-delta")
			return err
		}},
		{"VENUS goes offline", func() error { return env.Get("VENUS").Stop() }},
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			log.Printf("demo: %s: %v", s.what, err)
		} else {
			log.Printf("demo: %s", s.what)
		}
	}
}
