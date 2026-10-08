package config

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// CompiledStructure is Structure with its patterns compiled.
type CompiledStructure struct {
	Project    *regexp.Regexp // nil when no project pattern is set
	ShareType  *regexp.Regexp // nil when share types are not read from paths
	TypeTokens map[string]string
	Placement  []CompiledPlacement
	IgnoreDirs []string
	Depth      int
}

// CompiledPlacement is a compiled placement rule.
type CompiledPlacement struct {
	Source string
	When   *regexp.Regexp
	Must   map[string]string
}

// Compile validates and compiles the structure rules.
func (s Structure) Compile() (*CompiledStructure, error) {
	cs := &CompiledStructure{TypeTokens: map[string]string{}, IgnoreDirs: s.IgnoreDirs, Depth: s.NestedScanDepth}
	if cs.Depth <= 0 {
		cs.Depth = 3
	}
	flags := ""
	if s.CaseInsensitive {
		flags = "(?i)"
	}
	var err error
	if s.ProjectPattern != "" {
		if cs.Project, err = regexp.Compile(flags + s.ProjectPattern); err != nil {
			return nil, fmt.Errorf("structure.project_pattern: %v", err)
		}
	}
	if s.ShareTypeFromPath != "" {
		if cs.ShareType, err = regexp.Compile(s.ShareTypeFromPath); err != nil {
			return nil, fmt.Errorf("structure.share_type_from_path: %v", err)
		}
	}
	for _, t := range s.TypeTokens {
		t = strings.TrimSpace(t)
		if t != "" {
			cs.TypeTokens[strings.ToUpper(t)] = t
		}
	}
	for i, p := range s.Placement {
		re, err := regexp.Compile(flags + p.WhenNameMatches)
		if err != nil {
			return nil, fmt.Errorf("structure.placement[%d].when_name_matches: %v", i, err)
		}
		if len(p.ShareMustHave) == 0 {
			return nil, fmt.Errorf("structure.placement[%d]: share_must_have is empty", i)
		}
		cs.Placement = append(cs.Placement, CompiledPlacement{Source: p.WhenNameMatches, When: re, Must: p.ShareMustHave})
	}
	for _, g := range s.IgnoreDirs {
		if _, err := path.Match(g, ""); err != nil {
			return nil, fmt.Errorf("structure.ignore_dirs: bad pattern %q", g)
		}
	}
	return cs, nil
}

// IsProject reports whether a directory name matches the project pattern.
// With no pattern, every directory counts as a project folder.
func (cs *CompiledStructure) IsProject(name string) bool {
	if cs.Project == nil {
		return true
	}
	return cs.Project.MatchString(name)
}

// HasProjectPattern reports whether a project pattern is configured.
func (cs *CompiledStructure) HasProjectPattern() bool { return cs.Project != nil }

// Ignored reports whether a directory name is in ignore_dirs (glob patterns, case-insensitive).
func (cs *CompiledStructure) Ignored(name string) bool {
	ln := strings.ToLower(name)
	for _, g := range cs.IgnoreDirs {
		if ok, _ := path.Match(strings.ToLower(g), ln); ok {
			return true
		}
	}
	return false
}

// ShareAttrs extracts named groups from a share's directory name. ok is false
// when share types are configured but the name doesn't match.
func (cs *CompiledStructure) ShareAttrs(shareName string) (attrs map[string]string, ok bool) {
	attrs = map[string]string{}
	if cs.ShareType == nil {
		return attrs, true
	}
	m := cs.ShareType.FindStringSubmatch(shareName)
	if m == nil {
		return attrs, false
	}
	for i, n := range cs.ShareType.SubexpNames() {
		if n != "" && i < len(m) {
			attrs[n] = m[i]
		}
	}
	return attrs, true
}

// NameTokens returns the distinct type tokens found in a name, split on - and _.
func (cs *CompiledStructure) NameTokens(name string) []string {
	if len(cs.TypeTokens) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, seg := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' }) {
		up := strings.ToUpper(seg)
		if canon, ok := cs.TypeTokens[up]; ok && !seen[up] {
			seen[up] = true
			out = append(out, canon)
		}
	}
	return out
}

// AttrSatisfies reports whether a share attribute value satisfies a placement requirement.
// "true"/"false" test whether the named group matched something.
func AttrSatisfies(have string, want string) bool {
	switch strings.ToLower(strings.TrimSpace(want)) {
	case "true", "yes":
		return have != ""
	case "false", "no":
		return have == ""
	default:
		return strings.EqualFold(have, want)
	}
}
