package web

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

func TestSetupFlow(t *testing.T) {
	e := newEnv(t, twoServers)
	resp, _ := e.get("/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("expected redirect to setup, got %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	tok := e.web.SetupToken()
	csrf := e.csrf("/setup")
	if r := e.post("/setup", url.Values{"csrf": {csrf}, "token": {"wrong"}, "password": {"long enough pw"}, "password2": {"long enough pw"}}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token accepted: %d", r.StatusCode)
	}
	if r := e.post("/setup", url.Values{"csrf": {csrf}, "token": {tok}, "password": {"short"}, "password2": {"short"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password accepted: %d", r.StatusCode)
	}
	if r := e.post("/setup", url.Values{"csrf": {csrf}, "token": {tok}, "password": {"long enough pw"}, "password2": {"long enough pw"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup failed: %d", r.StatusCode)
	}
	if e.web.SetupToken() != "" {
		t.Fatal("setup token must be cleared")
	}
	resp, body := e.get("/settings")
	if resp.StatusCode != 200 || !strings.Contains(body, "Servers") {
		t.Fatalf("settings after setup: %d", resp.StatusCode)
	}
	for _, secret := range []string{"secret-key-akka", "secret-hook", "swt_test_token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("settings page shows secret %q", secret)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	csrf := e.csrf("/login")
	for i := 0; i < 5; i++ {
		if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"nope"}}); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}}); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected rate limit, got %d", r.StatusCode)
	}
}

func TestViewerModes(t *testing.T) {
	e := newEnv(t, twoServers+"access: {viewer: open}\n")
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	if resp, body := e.get("/"); resp.StatusCode != 200 || !strings.Contains(body, "AKKA") || !strings.Contains(body, "Log in") {
		t.Fatalf("open viewing: %d", resp.StatusCode)
	}
	if resp, _ := e.get("/settings"); resp.StatusCode == 200 {
		t.Fatal("anonymous viewer reached settings")
	}
	resp, _ := e.get("/frag/problems")
	if resp.StatusCode != 200 {
		t.Fatalf("fragment: %d", resp.StatusCode)
	}

	e2 := newEnv(t, twoServers)
	_ = e2.st.SetSetting(KeyAdminHash, h)
	if resp, _ := e2.get("/grid"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("admin-only: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := e2.get("/frag/grid"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("fragment without login: %d", resp.StatusCode)
	}

	e3 := newEnv(t, twoServers+"access: {mode: proxy}\n")
	if resp, _ := e3.get("/settings"); resp.StatusCode != 200 {
		t.Fatalf("proxy mode: %d", resp.StatusCode)
	}
}

// While notifications fail, every page says so, with the time of the next
// try when the notifier knows it; viewers don't see the error text.
func TestNotificationFailureBanner(t *testing.T) {
	e := adminEnv(t, twoServers+"access: {viewer: open}\ntimezone: Europe/Stockholm\n")
	now := time.Date(2026, 10, 11, 19, 44, 0, 0, time.UTC) // 21:44 in Stockholm
	e.web.now = func() time.Time { return now }
	const banner = "Notifications fail"
	if _, body := e.get("/"); strings.Contains(body, banner) {
		t.Fatal("banner while notifications work")
	}
	e.setDelivery(notify.Delivery{Failing: true, Since: now, Error: "discord: HTTP 404: Unknown Webhook", NextTry: now.Add(8*time.Minute + 30*time.Second)})
	const next = "syncwatch tries again at 21:52."
	for _, p := range []string{"/", "/grid", "/problems", "/settings", "/frag/alert"} {
		if _, body := e.get(p); !strings.Contains(body, banner) || !strings.Contains(body, "Unknown Webhook") || !strings.Contains(body, next) {
			t.Fatalf("%s: no banner with the error and the next try for the admin", p)
		}
	}
	anon, _ := http.Get(e.srv.URL + "/")
	b, _ := io.ReadAll(anon.Body)
	anon.Body.Close()
	if !strings.Contains(string(b), banner) || strings.Contains(string(b), "Unknown Webhook") || !strings.Contains(string(b), next) {
		t.Fatal("open viewer: want the banner with the next try, without the error text")
	}
	// A try time that passed, or none: the banner does not give a time.
	for _, try := range []time.Time{now.Add(-time.Second), {}} {
		e.setDelivery(notify.Delivery{Failing: true, Since: now, Error: "discord: HTTP 500", NextTry: try})
		if _, body := e.get("/frag/alert"); !strings.Contains(body, "syncwatch tries again automatically.") || strings.Contains(body, "tries again at") {
			t.Errorf("next try %v: %s", try, body)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ := http.DefaultClient.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"failing": true`) {
		t.Fatalf("API: %s", b)
	}
	e.setDelivery(notify.Delivery{})
	if _, body := e.get("/frag/alert"); strings.Contains(body, banner) || !strings.Contains(body, `id="sw-alert"`) {
		t.Fatalf("alert fragment after recovery: %s", body)
	}
}

func TestSecurityHeadersAndAssets(t *testing.T) {
	e := newEnv(t, twoServers)
	resp, _ := e.get("/login")
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers: %v", resp.Header)
	}
	if pp := resp.Header.Get("Permissions-Policy"); !strings.Contains(pp, "camera=()") || strings.Contains(pp, "interest-cohort") {
		t.Fatalf("Permissions-Policy: %q", pp)
	}
	for _, a := range []string{"/assets/htmx.min.js", "/assets/htmx-ext-sse.js", "/assets/app.css", "/assets/app.js", "/assets/syncwatch-icon.svg", "/assets/syncwatch-icon-32.png", "/assets/syncwatch-icon-256.png", "/favicon.ico"} {
		if r, _ := e.get(a); r.StatusCode != 200 {
			t.Fatalf("%s: %d", a, r.StatusCode)
		}
	}
	if r, _ := e.get("/healthz"); r.StatusCode != 200 {
		t.Fatal("healthz")
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("pässwörd with spaces")
	if err != nil || !strings.HasPrefix(h, "$argon2id$") {
		t.Fatal(h, err)
	}
	if !CheckPassword("pässwörd with spaces", h) || CheckPassword("password", h) || CheckPassword("x", "garbage") {
		t.Fatal("check")
	}
}

// Flash messages survive non-ASCII text and long errors.
func TestFlashCookie(t *testing.T) {
	for _, msg := range []string{"Saved; now: 3, \"quoted\"\nand… 100% åäö", strings.Repeat("Fel på servern – ", 400)} {
		enc := flashEncode(msg)
		if len(enc) > maxFlash {
			t.Fatalf("encoded flash is %d bytes", len(enc))
		}
		for i := 0; i < len(enc); i++ {
			if c := enc[i]; c < 0x21 || c > 0x7e || c == ';' || c == ',' || c == '"' || c == '\\' {
				t.Fatalf("byte %q isn't allowed in a cookie value", c)
			}
		}
		dec := flashDecode(enc)
		if !utf8.ValidString(dec) {
			t.Fatal("decoded flash isn't valid UTF-8")
		}
		if len(msg) < 100 && dec != msg {
			t.Fatalf("round trip: %q", dec)
		}
		if len(msg) > 1000 && (!strings.HasSuffix(dec, "…") || !strings.HasPrefix(msg, strings.TrimSuffix(dec, "…"))) {
			t.Fatalf("long message not cut cleanly: …%q", dec[len(dec)-20:])
		}
	}
	e := adminEnv(t, twoServers+"timezone: Europe/Stockholm\n")
	e.web.now = func() time.Time { return time.Date(2026, 10, 4, 22, 30, 0, 0, time.UTC) }
	if resp, _ := e.get("/settings/export"); !strings.Contains(resp.Header.Get("Content-Disposition"), "syncwatch-2026-10-05.yaml") {
		t.Fatalf("export file name: %q", resp.Header.Get("Content-Disposition"))
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/grid": "/grid", "/problems?severity=error&server=akka": "/problems?severity=error&server=akka", "/problems#f-x": "/problems#f-x",
		"//evil.com": "/", "https://evil.com": "/", "/\\evil": "/", "": "/", "grid": "/",
		// Browsers strip tab, CR and LF, which would turn these into //evil.com.
		"/\t/evil.com": "/", "/\n/evil.com": "/", "/\r/evil.com": "/", "/a\\b": "/", "/\x7f": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// A crafted login link must not send the admin to another site.
func TestLoginRedirectStaysOnSite(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	_, body := e.get("/login?next=" + url.QueryEscape("/\t/evil.example"))
	if strings.Contains(body, "evil.example") {
		t.Fatal("login form carries the foreign next value")
	}
	csrf := e.csrf("/login")
	r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}, "next": {"/\t/evil.example"}})
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("login redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}

// The setup token is logged at WARN. When the log level hides warnings it
// is printed to stderr as well, since the first start can't finish without it.
func TestSetupTokenPrintedWhenLogHidesWarnings(t *testing.T) {
	e := newEnv(t, twoServers)
	var logBuf, errBuf bytes.Buffer
	e.web.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))
	e.web.LogSetupToken(&errBuf)
	tok := e.web.SetupToken()
	if tok == "" || !strings.Contains(errBuf.String(), tok) || logBuf.Len() != 0 {
		t.Fatalf("log level error: stderr %q, log %q", errBuf.String(), logBuf.String())
	}
	errBuf.Reset()
	e.web.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	e.web.LogSetupToken(&errBuf)
	if !strings.Contains(logBuf.String(), tok) || errBuf.Len() != 0 {
		t.Fatalf("log level info: stderr %q, log %q", errBuf.String(), logBuf.String())
	}
	// Nothing once an admin password exists, or in proxy mode.
	logBuf.Reset()
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	e.web.LogSetupToken(&errBuf)
	e2 := newEnv(t, twoServers+"access: {mode: proxy}\n")
	e2.web.log = e.web.log
	e2.web.LogSetupToken(&errBuf)
	if logBuf.Len() != 0 || errBuf.Len() != 0 {
		t.Fatalf("admin set or proxy mode: stderr %q, log %q", errBuf.String(), logBuf.String())
	}
}

// A form posted after the session expired goes to the login page without a
// next to the POST-only URL (a GET there would be 405); pages keep their
// next. A visit to /login with a session goes straight to next.
func TestLoginNextOnlyForPages(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	csrf := e.csrf("/login")
	if r := e.post("/settings/servers", url.Values{"csrf": {csrf}, "name": {"X"}}); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous POST: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := e.get("/problems?severity=error"); r.Header.Get("Location") != "/login?next=/problems%3Fseverity=error" {
		t.Fatalf("anonymous GET: %q", r.Header.Get("Location"))
	}
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}, "next": {"/grid"}}); r.Header.Get("Location") != "/grid" {
		t.Fatalf("login: %q", r.Header.Get("Location"))
	}
	if r, _ := e.get("/login?next=/problems"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/problems" {
		t.Fatalf("/login with a session: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := e.get("/login"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("/login with a session and no next: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}

// A viewer session from before viewing was switched off gives no role. The
// login page shows the form to it; sending it on to a page would send it
// back to /login, round and round.
func TestLoginPageWithStaleViewerSession(t *testing.T) {
	e := newEnv(t, twoServers+"access: {viewer: password}\n")
	ah, _ := HashPassword("correct horse battery")
	vh, _ := HashPassword("viewer password 1")
	_ = e.st.SetSetting(KeyAdminHash, ah)
	_ = e.st.SetSetting(KeyViewerHash, vh)
	csrf := e.csrf("/login")
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"viewer password 1"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("viewer login: %d", r.StatusCode)
	}
	c := e.cfg.Get().Clone()
	c.Access.Viewer = config.ViewerNone
	if err := e.cfg.Set(c); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.get("/"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login?next=/" {
		t.Fatalf("page with a stale viewer session: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, body := e.get("/login?next=/"); r.StatusCode != http.StatusOK || !strings.Contains(body, `name="password"`) {
		t.Fatalf("/login with a stale viewer session: %d, want the login form", r.StatusCode)
	}
	csrf = e.csrf("/login?next=/") // the viewer login made a new CSRF token
	// The admin password still gets in.
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}, "next": {"/"}}); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("admin login: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := e.get("/settings"); r.StatusCode != http.StatusOK {
		t.Fatalf("/settings after the admin login: %d", r.StatusCode)
	}
}

// Severity words are the same everywhere in the dashboard: badges, the
// filter and the summary line say error, warning and suggestion. Counts
// get their noun in the right number.
func TestSeverityWordsAndPlurals(t *testing.T) {
	e := adminEnv(t, twoServers) // two unreachable servers: two H1 errors
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID = model.StatusUp, true, "AAAAAAA-BBBBBBB"
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: "only-folder", State: "idle"}}
	})
	e.eng.Evaluate()
	_, body := e.get("/problems")
	for _, want := range []string{"🔴 ERROR</span>", `<option value="warn" >WARNING</option>`, `<option value="info" >INFO</option>`,
		"🔴 ERROR 1</span>", "🟠 WARNING 0</span>", "🔵 INFO 0</span>"} {
		if !strings.Contains(body, want) {
			t.Errorf("problems page lacks %q", want)
		}
	}
	for _, old := range []string{">WARN<", ">warn<", ">info<", "1 errors", "suggestion"} {
		if strings.Contains(body, old) {
			t.Errorf("problems page still says %q", old)
		}
	}
	if _, grid := e.get("/grid"); !strings.Contains(grid, "1 folder</span>") || strings.Contains(grid, "1 folders") {
		t.Error("grid: a group with one folder says folders")
	}
	for n, want := range map[int]string{0: "0 errors", 1: "1 error", 2: "2 errors"} {
		if got := plural(n, "error", "errors"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
	if model.Error.Label() != "ERROR" || model.Warn.Label() != "WARNING" || model.Info.Label() != "INFO" {
		t.Error("severity labels")
	}
	if model.ParseSeverity("suggestion") != model.Info || model.ParseSeverity("warning") != model.Warn {
		t.Error("ParseSeverity does not accept the dashboard words")
	}
}

// "Snooze until date" covers the whole chosen day in the configured zone,
// today included, and takes a reason like ignore does.
func TestSnoozeUntilDate(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: Pacific/Auckland\n")
	auckland := e.cfg.Get().Location()
	// 10:00 UTC is 23:00 in Auckland (NZDT): still 2026-10-10 there.
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	e.web.now = func() time.Time { return now }
	id := e.eng.View().Open[0].ID
	csrf := e.csrf("/problems")
	msg, isErr := flash(e.post("/problems/"+id+"/snooze", url.Values{"csrf": {csrf}, "for": {"date"}, "until": {"2026-10-10"}, "reason": {"waiting for the disk"}}))
	if isErr {
		t.Fatalf("snoozing until today refused: %s", msg)
	}
	z, ok := e.eng.View().Snoozes[id]
	if want := time.Date(2026, 10, 11, 0, 0, 0, 0, auckland); !ok || !z.Until.Equal(want) || z.Reason != "waiting for the disk" {
		t.Fatalf("snooze = %+v, want until %v with the reason", z, want)
	}
	if msg != "The finding is snoozed until 2026-10-11 00:00." {
		t.Errorf("flash %q", msg)
	}
	if msg, isErr := flash(e.post("/problems/"+id+"/snooze", url.Values{"csrf": {csrf}, "for": {"date"}, "until": {"2026-10-09"}})); !isErr {
		t.Errorf("yesterday accepted: %q", msg)
	}
	_, body := e.get("/problems")
	if !strings.Contains(body, `name="until" min="2026-10-10"`) {
		t.Error("the date field does not start at today")
	}
	i := strings.Index(body, "/snooze\"")
	if j := strings.Index(body[i:], "/ignore\""); i < 0 || j < 0 || !strings.Contains(body[i:i+j], `name="reason"`) {
		t.Error("the snooze form has no reason field")
	}
}

// "Resolved this week" counts every resolution of the last 7 days and
// lists the latest 30, newest first.
func TestResolvedThisWeekCountAndOrder(t *testing.T) {
	e := adminEnv(t, twoServers)
	now := time.Now()
	e.web.now = func() time.Time { return now }
	// Opened in order 00..34 three weeks ago; 00 resolved most recently.
	for i := 0; i < 35; i++ {
		r := &store.Record{Finding: model.Finding{ID: fmt.Sprintf("h%02d", i), Check: "H7", Severity: model.Warn, Message: fmt.Sprintf("resolved-%02d", i)},
			OpenedAt: now.Add(-21*24*time.Hour + time.Duration(i)*time.Hour)}
		if err := e.st.HistoryOpen(r); err != nil {
			t.Fatal(err)
		}
		if err := e.st.HistoryResolve(r.ID, now.Add(-time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	_, body := e.get("/problems")
	if !strings.Contains(body, "35 resolved this week") || !strings.Contains(body, "Resolved this week (35") {
		t.Error("the summary does not show the real count")
	}
	first, second, dropped := strings.Index(body, "resolved-00"), strings.Index(body, "resolved-01"), strings.Index(body, "resolved-30")
	if first < 0 || second < first || dropped >= 0 {
		t.Errorf("list order: resolved-00 at %d, resolved-01 at %d, resolved-30 at %d (want newest first, oldest five dropped)", first, second, dropped)
	}
}

// Ignored and snoozed findings stay visible: a collapsed list on the
// problems page with a way back, and a count on the overview.
func TestSnoozedAndIgnoredStayVisible(t *testing.T) {
	e := adminEnv(t, twoServers)
	id := e.eng.View().Open[0].ID
	csrf := e.csrf("/problems")
	e.post("/problems/"+id+"/ignore", url.Values{"csrf": {csrf}, "reason": {"known and harmless"}})
	_, body := e.get("/problems")
	for _, want := range []string{"Snoozed and ignored (1)", "ignored: known and harmless", "/problems/" + id + "/unsnooze", "Stop ignoring", `id="f-` + id + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("problems page lacks %q", want)
		}
	}
	if _, ov := e.get("/"); !strings.Contains(ov, "1 snoozed or ignored") {
		t.Error("overview does not mention the ignored finding")
	}
	// With "Show snoozed and ignored" on, the rows are inline and the block is gone.
	_, body = e.get("/problems?snoozed=1")
	if strings.Contains(body, "Snoozed and ignored (") || !strings.Contains(body, "ignored: known and harmless") {
		t.Error("snoozed=1: want the ignored finding inline, no separate block")
	}
}

// A live update must not wipe an ignore reason or snooze date someone is
// typing: the shipped script cancels the swap while a form inside the
// updated section is in use. The behaviour itself was checked in a headless
// browser; this pins the asset.
func TestLiveUpdateGuardShipped(t *testing.T) {
	e := newEnv(t, twoServers)
	_, js := e.get("/assets/app.js")
	for _, want := range []string{`"htmx:beforeSwap"`, "shouldSwap = false", `htmx.trigger(t, "sse:update")`, `"focusout"`} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	if _, css := e.get("/assets/app.css"); !strings.Contains(css, ".updates-paused") {
		t.Error("app.css has no style for a paused section")
	}
}
