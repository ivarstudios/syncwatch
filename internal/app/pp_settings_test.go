package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/web"
)

// SYNCWATCH_ADMIN_PASSWORD is not used when it is the viewer
// password (a viewer password without an administrator password exists when
// it was set in proxy mode). The login checks the administrator password
// first: with one password for both, each login with the viewer password is
// an administrator session.
func TestAdminPasswordFromEnvironmentIsNotTheViewerPassword(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	vh, _ := web.HashPassword("studio lobby 2026")
	if err := st.SetSetting(web.KeyViewerHash, vh); err != nil {
		t.Fatal(err)
	}
	st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{DataDir: dir, Listen: "127.0.0.1:0", AdminPassword: "studio lobby 2026",
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), OnListen: func(a string) { addr <- a }})
	}()
	select {
	case err := <-done:
		if want := "SYNCWATCH_ADMIN_PASSWORD must be different from the viewer password"; err == nil || err.Error() != want {
			t.Fatalf("Run: %v, want %q", err, want)
		}
	case <-addr:
		cancel()
		<-done
		t.Fatal("syncwatch started with the viewer password as the administrator password")
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	st, err = OpenExistingStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if h, _ := st.Setting(web.KeyAdminHash); h != "" {
		t.Fatal("the viewer password is the administrator password")
	}
}
