package harness

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// freePort asks the OS for a free loopback port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startInstance generates and starts one Syncthing on free ports.
func startInstance(t *testing.T) (*Instance, context.Context) {
	t.Helper()
	bin := os.Getenv("SYNCTHING_BIN")
	if bin == "" {
		t.Skip("SYNCTHING_BIN not set")
	}
	inst := newInstance(bin, t.TempDir(), "HARNESS", true, freePort(t), freePort(t))
	if err := inst.generate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := inst.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Stop() })
	return inst, ctx
}

// TestGenerateIsolates checks the config patches against the real binary: a
// generated instance has everything that reaches outside the test network
// turned off.
func TestGenerateIsolates(t *testing.T) {
	inst, ctx := startInstance(t)
	if err := inst.CheckIsolated(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestStopKillsProcessTree checks that Stop frees the ports even when the API
// shutdown fails and the process has to be killed: Syncthing runs as a
// launcher plus a worker child that holds the ports, so killing the launcher
// alone would leave the worker running.
func TestStopKillsProcessTree(t *testing.T) {
	inst, _ := startInstance(t)
	old := stopGrace
	stopGrace = 2 * time.Second
	defer func() { stopGrace = old }()
	// Make the API shutdown fail so Stop has to kill the process.
	inst.APIKey = "not-the-key"
	if err := inst.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, p := range []int{inst.GUIPort, inst.SyncPort} {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			t.Fatalf("port %d still in use after Stop: %v", p, err)
		}
		ln.Close()
	}
}
