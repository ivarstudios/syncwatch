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

func TestRoots(t *testing.T) {
	u, w := StyleFor("/"), StyleFor(`\`)
	for _, c := range []struct {
		s    Style
		p    string
		want string
	}{
		{u, "/", ""},
		{u, "/x", "/"},
		{u, "/x/y/", "/x"},
		{w, `C:\`, ""},
		{w, `C:\x`, `C:\`},
		{w, `\\srv\share\dir\sub`, `\\srv\share\dir`},
		{w, `\\srv\share\dir`, `\\srv\share`},
		{w, `\\srv\share`, ""},
		{w, `\\srv`, ""},
		{w, `//srv/share/dir`, `\\srv\share`},
	} {
		if got := c.s.Parent(c.p); got != c.want {
			t.Errorf("Parent(%q) = %q, want %q", c.p, got, c.want)
		}
	}
	if u.Base("/") != "" || w.Base(`\\srv\share`) != "share" || w.Base(`C:\`) != "" {
		t.Error("base of roots")
	}
	if !u.Within("/x", "/") || u.Rel("/x/y", "/") != "x/y" || !w.Within(`\\srv\share\x`, `\\srv\share`) || !w.Within(`C:\x`, `C:\`) {
		t.Error("within/rel of roots")
	}
}

func TestCleanCollapsesSeparators(t *testing.T) {
	u, w := StyleFor("/"), StyleFor(`\`)
	for _, c := range []struct {
		s     Style
		p     string
		want  string
		equal string
	}{
		{u, "/a//b/", "/a/b", "/a/b"},
		{u, "//a///b", "/a/b", "/a/b"},
		{u, "//", "/", "/"},
		{w, `C:\a\\b\`, `C:\a\b`, `c:/a/b`},
		{w, `\\srv\\share\\dir\`, `\\srv\share\dir`, `\\SRV\share\dir`},
		{w, `\\\\srv\share`, `\\srv\share`, `\\srv\share`},
	} {
		if got := c.s.Clean(c.p); got != c.want {
			t.Errorf("Clean(%q) = %q, want %q", c.p, got, c.want)
		}
		if !c.s.Equal(c.p, c.equal) {
			t.Errorf("Equal(%q, %q) is false", c.p, c.equal)
		}
	}
	if !u.Within("/a//b/c", "/a/b") || u.Rel("/a//b/c", "/a/b/") != "c" {
		t.Error("within/rel with doubled separators")
	}
	// A backslash is an ordinary character on a Linux server.
	if u.Clean(`/a\\b`) != `/a\\b` || u.Display(`a\b`) != `a\b` {
		t.Error("linux backslash")
	}
}
