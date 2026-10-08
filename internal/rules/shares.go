package rules

import (
	"sort"
	"strings"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/pathx"
)

// ShareRef is a share directory on a server.
type ShareRef struct {
	Path   string
	Name   string
	Attrs  map[string]string
	Manual bool
}

// DetectShares works out a server's shares from its Syncthing folder paths.
//
// Without share_type_from_path, each folder's parent directory is a share.
// With it, the nearest ancestor (or the folder itself) whose name matches the
// pattern is the share, so folders in unrelated places don't create shares.
// Manual additions are added and exclusions removed.
func DetectShares(sv *model.Server, add, exclude []string, cs *config.CompiledStructure) []ShareRef {
	st := pathx.StyleFor(sv.PathSep)
	found := map[string]ShareRef{}
	key := func(p string) string {
		p = st.Clean(p)
		if st.CaseInsensitive {
			return strings.ToLower(p)
		}
		return p
	}
	put := func(p string, manual bool) {
		p = st.Clean(p)
		if p == "" {
			return
		}
		k := key(p)
		if old, ok := found[k]; ok {
			old.Manual = old.Manual || manual
			found[k] = old
			return
		}
		name := st.Base(p)
		attrs, _ := cs.ShareAttrs(name)
		found[k] = ShareRef{Path: p, Name: name, Attrs: attrs, Manual: manual}
	}
	for _, f := range sv.Folders {
		if f.Path == "" {
			continue
		}
		if cs.ShareType == nil {
			if parent := st.Parent(f.Path); parent != "" && parent != st.Sep {
				put(parent, false)
			}
			continue
		}
		for p := st.Clean(f.Path); p != "" && p != st.Sep; p = st.Parent(p) {
			if _, ok := cs.ShareAttrs(st.Base(p)); ok {
				put(p, false)
				break
			}
			if st.Parent(p) == p {
				break
			}
		}
	}
	for _, p := range add {
		put(p, true)
	}
	for _, p := range exclude {
		delete(found, key(p))
	}
	out := make([]ShareRef, 0, len(found))
	for _, s := range found {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ShareOf returns the share containing a path, or nil.
func ShareOf(shares []ShareRef, p string, st pathx.Style) *ShareRef {
	var best *ShareRef
	for i := range shares {
		s := &shares[i]
		if st.Within(p, s.Path) || st.Equal(p, s.Path) {
			if best == nil || len(s.Path) > len(best.Path) {
				best = s
			}
		}
	}
	return best
}
