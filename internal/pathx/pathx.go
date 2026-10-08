// Package pathx handles paths reported by a Syncthing server, which may use
// either / or \ as separator regardless of where syncwatch runs.
package pathx

import "strings"

// Style describes a server's path conventions.
type Style struct {
	Sep             string // "/" or "\\"
	CaseInsensitive bool   // true on Windows servers
}

// StyleFor returns the style for a server's path separator.
func StyleFor(sep string) Style {
	if sep == `\` {
		return Style{Sep: `\`, CaseInsensitive: true}
	}
	return Style{Sep: "/"}
}

// Clean removes trailing separators (keeping a root such as "/" or "C:\").
func (s Style) Clean(p string) string {
	if s.Sep == `\` {
		p = strings.ReplaceAll(p, "/", `\`)
	}
	for len(p) > 1 && strings.HasSuffix(p, s.Sep) {
		if s.Sep == `\` && len(p) == 3 && p[1] == ':' {
			break
		}
		p = p[:len(p)-1]
	}
	return p
}

// Join joins path elements with the separator.
func (s Style) Join(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			if !strings.HasSuffix(b.String(), s.Sep) {
				b.WriteString(s.Sep)
			}
			p = strings.TrimPrefix(p, s.Sep)
		}
		b.WriteString(p)
	}
	return b.String()
}

// Base returns the last element.
func (s Style) Base(p string) string {
	p = s.Clean(p)
	if i := strings.LastIndex(p, s.Sep); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Parent returns the parent directory, or "" when there is none.
func (s Style) Parent(p string) string {
	p = s.Clean(p)
	i := strings.LastIndex(p, s.Sep)
	if i < 0 {
		return ""
	}
	if i == 0 {
		return s.Sep
	}
	if s.Sep == `\` && i == 2 && p[1] == ':' {
		return p[:3]
	}
	return p[:i]
}

// Equal compares two paths.
func (s Style) Equal(a, b string) bool {
	a, b = s.Clean(a), s.Clean(b)
	if s.CaseInsensitive {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Within reports whether child is strictly inside parent.
func (s Style) Within(child, parent string) bool {
	child, parent = s.Clean(child), s.Clean(parent)
	if s.CaseInsensitive {
		child, parent = strings.ToLower(child), strings.ToLower(parent)
	}
	if !strings.HasSuffix(parent, s.Sep) {
		parent += s.Sep
	}
	return len(child) > len(parent) && strings.HasPrefix(child, parent)
}

// Rel returns child relative to parent, or child unchanged when it isn't inside.
func (s Style) Rel(child, parent string) string {
	if !s.Within(child, parent) {
		return child
	}
	pc := s.Clean(parent)
	if !strings.HasSuffix(pc, s.Sep) {
		pc += s.Sep
	}
	return s.Clean(child)[len(pc):]
}

// Split returns the elements of a relative path.
func (s Style) Split(rel string) []string {
	return strings.FieldsFunc(rel, func(r rune) bool { return string(r) == s.Sep || r == '/' })
}

// Display converts a relative path to use / for display.
func (s Style) Display(rel string) string { return strings.ReplaceAll(rel, `\`, "/") }
