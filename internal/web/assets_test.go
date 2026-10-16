package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// Each link to a file in assets has the version of the content of the
// file, thus a new version of syncwatch gives new URLs and no browser keeps
// an old stylesheet or script from its cache. The URL with the version
// serves the file.
func TestAssetURLsHaveTheContentVersion(t *testing.T) {
	e := adminEnv(t, twoServers)
	css, err := assetFS.ReadFile("assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(css)
	want := "/assets/app.css?v=" + hex.EncodeToString(sum[:5])
	if got := assetURL("app.css"); got != want {
		t.Fatalf("assetURL = %q, want %q", got, want)
	}
	for _, path := range []string{"/grid", "/lobby"} {
		_, page := e.get(path)
		refs := regexp.MustCompile(`(?:src|href)="(/assets/[^"]*)"`).FindAllStringSubmatch(page, -1)
		if len(refs) == 0 {
			t.Fatalf("%s: no assets", path)
		}
		for _, m := range refs {
			if !regexp.MustCompile(`^/assets/[A-Za-z0-9._-]+\?v=[0-9a-f]{10}$`).MatchString(m[1]) {
				t.Errorf("%s: %s has no version", path, m[1])
			}
		}
	}
	resp, body := e.get(want)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "table.grid") || resp.Header.Get("Cache-Control") != "public, max-age=86400" {
		t.Errorf("%s: %d, Cache-Control %q", want, resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

// The Folders page uses the whole window; the other pages keep their width.
func TestFoldersPageIsWide(t *testing.T) {
	e := adminEnv(t, twoServers)
	if _, page := e.get("/grid"); !strings.Contains(page, `<main class="wide">`) {
		t.Error("the Folders page is not wide")
	}
	if _, page := e.get("/problems"); !strings.Contains(page, "<main>") {
		t.Error("the Findings page is wide")
	}
}
