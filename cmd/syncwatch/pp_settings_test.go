package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ivarstudios/syncwatch/internal/app"
	"github.com/ivarstudios/syncwatch/internal/web"
)

// The command reset-admin-password refuses the viewer password. The login
// checks the administrator password first: with one password for both, each
// login with the viewer password is an administrator session.
func TestResetPasswordRefusesTheViewerPassword(t *testing.T) {
	t.Setenv("SYNCWATCH_SECRET_KEY", "")
	dir := t.TempDir()
	st, err := app.OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	ah, _ := web.HashPassword("correct horse battery")
	vh, _ := web.HashPassword("studio lobby 2026")
	if err := st.SetSetting(web.KeyAdminHash, ah); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(web.KeyViewerHash, vh); err != nil {
		t.Fatal(err)
	}
	st.Close()
	in := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(in, []byte("studio lobby 2026\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = stdin }()
	err = resetPassword([]string{"--data", dir})
	if want := "the administrator password must be different from the viewer password"; err == nil || err.Error() != want {
		t.Fatalf("reset-admin-password with the viewer password: %v, want %q", err, want)
	}
	st, err = app.OpenExistingStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if h, _ := st.Setting(web.KeyAdminHash); !web.CheckPassword("correct horse battery", h) {
		t.Fatal("the refused password replaced the administrator password")
	}
	if notices, _ := st.SettingsWithPrefix("cli.notice."); len(notices) != 0 {
		t.Fatalf("a refused reset recorded a settings notice: %v", notices)
	}
}
