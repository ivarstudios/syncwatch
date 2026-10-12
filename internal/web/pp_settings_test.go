package web

// Tests of the pre-publication fixes of the Settings area (pp-settings).

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
)

const (
	ppAdminPW  = "correct horse battery" // the administrator password of adminEnv
	ppViewerPW = "studio lobby 2026"     // the staff and the lobby screen use it
)

// ppLogin logs in with pw from a browser of its own and returns the role of
// the session that syncwatch stored for it ("" when the login failed).
func ppLogin(t *testing.T, e *env, pw string) string {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := csrfRe.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("no csrf token on /login")
	}
	if resp, err = c.PostForm(e.srv.URL+"/login", url.Values{"csrf": {m[1]}, "password": {pw}}); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, _ := url.Parse(e.srv.URL)
	for _, ck := range jar.Cookies(u) {
		if ck.Name == sessionCookie {
			if z, err := e.st.Session(hashToken(ck.Value), time.Now()); err == nil && z != nil {
				return z.Role
			}
		}
	}
	return ""
}

// sessions-1: the viewer password and the administrator password are
// different. The login checks the administrator password first: with one
// password for both, each login with the viewer password (the staff, the
// lobby screen) was an administrator session.
func TestViewerAndAdministratorPasswordsDiffer(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	access := func(f url.Values) (string, bool) {
		f.Set("csrf", csrf)
		f.Set("mode", "builtin")
		return flash(e.post("/settings/access", f))
	}

	// The administrator password as the viewer password.
	msg, isErr := access(url.Values{"viewer": {"password"}, "viewer_password": {ppAdminPW}, "confirm": {ppAdminPW}})
	if !isErr || msg != "The viewer password must be different from the administrator password." {
		t.Fatalf("the administrator password as the viewer password: %q (error %v)", msg, isErr)
	}
	if _, ok := e.web.setting(KeyViewerHash); ok || e.cfg.Get().Access.Viewer != config.ViewerNone {
		t.Fatalf("the refused change was stored: viewer hash %v, viewer access %q", ok, e.cfg.Get().Access.Viewer)
	}

	// A different viewer password: the lobby screen is a viewer.
	if msg, isErr := access(url.Values{"viewer": {"password"}, "viewer_password": {ppViewerPW}, "confirm": {ppAdminPW}}); isErr {
		t.Fatalf("a viewer password: %q", msg)
	}
	if role := ppLogin(t, e, ppViewerPW); role != RoleViewer {
		t.Fatalf("login with the viewer password: role %q", role)
	}

	// The viewer password as the new administrator password.
	notices := len(e.noticeList())
	msg, isErr = flash(e.post("/settings/password", url.Values{"csrf": {csrf}, "current": {ppAdminPW}, "password": {ppViewerPW}, "password2": {ppViewerPW}}))
	if !isErr || msg != "The administrator password must be different from the viewer password." {
		t.Fatalf("the viewer password as the administrator password: %q (error %v)", msg, isErr)
	}
	if h, _ := e.web.setting(KeyAdminHash); !CheckPassword(ppAdminPW, h) {
		t.Fatal("the refused administrator password was stored")
	}
	if n := e.noticeList(); len(n) != notices {
		t.Fatalf("a refused change sent a settings notice: %q", n[notices:])
	}
	if role := ppLogin(t, e, ppViewerPW); role != RoleViewer {
		t.Fatalf("login with the viewer password after the refused change: role %q", role)
	}
	if role := ppLogin(t, e, ppAdminPW); role != RoleAdmin {
		t.Fatalf("login with the administrator password: role %q", role)
	}
}

// sessions-1: the check of a new viewer password must not tell a session
// that does not know the administrator password (an unlocked browser, a
// copied cookie) whether a guess is the administrator password. Thus a new
// viewer password needs the administrator password first, also when it has
// no effect now (viewer access "administrator only"), and the comparison
// comes after that confirmation, inside its rate limit.
func TestViewerPasswordIsNoAdministratorPasswordOracle(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	answer := func(guess string) string {
		msg, isErr := flash(e.post("/settings/access", url.Values{"csrf": {csrf}, "mode": {"builtin"}, "viewer": {"none"}, "viewer_password": {guess}}))
		return fmt.Sprintf("%q (error %v)", msg, isErr)
	}
	wrong, right := answer("a wrong guess 01"), answer(ppAdminPW)
	if wrong != right {
		t.Fatalf("a wrong guess gives %s, the administrator password gives %s", wrong, right)
	}
	if want := fmt.Sprintf("%q (error true)", "This change permits access to more people. Type the administrator password to accept the change."); right != want {
		t.Fatalf("a new viewer password without the administrator password: %s, want %s", right, want)
	}
	if _, ok := e.web.setting(KeyViewerHash); ok {
		t.Fatal("a viewer password was stored without the administrator password")
	}
	// With the administrator password, a viewer password for later is saved.
	msg, isErr := flash(e.post("/settings/access", url.Values{"csrf": {csrf}, "mode": {"builtin"}, "viewer": {"none"}, "viewer_password": {ppViewerPW}, "confirm": {ppAdminPW}}))
	if h, ok := e.web.setting(KeyViewerHash); isErr || !ok || !CheckPassword(ppViewerPW, h) {
		t.Fatalf("a viewer password with the administrator password: %q (error %v)", msg, isErr)
	}
}

// sessions-1: the first start (/setup) refuses an administrator password
// that is the viewer password. A viewer password without an administrator
// password exists when it was set in proxy mode; syncwatch import-config
// can then set built-in login to on.
func TestSetupRefusesTheViewerPassword(t *testing.T) {
	e := newEnv(t, twoServers)
	vh, _ := HashPassword(ppViewerPW)
	if err := e.st.SetSetting(KeyViewerHash, vh); err != nil {
		t.Fatal(err)
	}
	token := e.web.SetupToken()
	csrf := e.csrf("/setup")
	resp, body := e.postPage("/setup", url.Values{"csrf": {csrf}, "token": {token}, "password": {ppViewerPW}, "password2": {ppViewerPW}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "The administrator password must be different from the viewer password.") {
		t.Fatalf("setup with the viewer password: %d %s", resp.StatusCode, body)
	}
	if e.web.adminConfigured() {
		t.Fatal("the viewer password is the administrator password")
	}
	if resp, _ := e.postPage("/setup", url.Values{"csrf": {csrf}, "token": {token}, "password": {ppAdminPW}, "password2": {ppAdminPW}}); resp.StatusCode != http.StatusSeeOther || !e.web.adminConfigured() {
		t.Fatalf("setup with a different password: %d", resp.StatusCode)
	}
}
