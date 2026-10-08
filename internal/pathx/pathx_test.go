package pathx

import "testing"

func TestUnix(t *testing.T) {
	s := StyleFor("/")
	if !s.Within("/share/A/b", "/share/A") || s.Within("/share/AB", "/share/A") || s.Within("/share/A", "/share/A") {
		t.Fatal("within")
	}
	if s.Base("/share/A/") != "A" || s.Parent("/share/A") != "/share" || s.Parent("/x") != "/" {
		t.Fatal("base/parent")
	}
	if s.Rel("/share/A/b/c", "/share/A") != "b/c" || s.Join("/share", "A", "b") != "/share/A/b" {
		t.Fatal("rel/join")
	}
	if s.Equal("/a/B", "/a/b") {
		t.Fatal("unix is case-sensitive")
	}
}

func TestWindows(t *testing.T) {
	s := StyleFor(`\`)
	if !s.Within(`D:\Share\a\B`, `d:\share\A`) || !s.Equal(`D:\x\`, `d:\X`) {
		t.Fatal("windows case-insensitive")
	}
	if s.Parent(`D:\x`) != `D:\` || s.Base(`D:\x\y`) != "y" || s.Clean(`D:\`) != `D:\` {
		t.Fatal("parent/base/clean")
	}
	if s.Join(`D:\a`, "b") != `D:\a\b` || s.Display(`a\b`) != "a/b" {
		t.Fatal("join/display")
	}
}
