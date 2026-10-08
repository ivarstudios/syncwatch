package rules

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/pathx"
)

func (e *evaluator) cross() {
	e.sameLabelDifferentIDs()
	e.folderShareTypes()
}

// X1: the same label with different folder IDs on two servers usually means a
// folder was added separately on each server instead of being shared.
func (e *evaluator) sameLabelDifferentIDs() {
	type loc struct{ server, id string }
	byLabel := map[string][]loc{}
	for _, sv := range e.Snapshot.Servers {
		if !sv.Loaded {
			continue
		}
		for _, f := range sv.Folders {
			if f.Label == "" {
				continue
			}
			k := strings.ToLower(f.Label)
			byLabel[k] = append(byLabel[k], loc{sv.ID, f.ID})
		}
	}
	for _, k := range sortedKeys(byLabel) {
		locs := byLabel[k]
		ids := map[string]bool{}
		servers := map[string]bool{}
		for _, l := range locs {
			ids[l.id] = true
			servers[l.server] = true
		}
		if len(ids) < 2 || len(servers) < 2 {
			continue
		}
		var parts []string
		for _, l := range locs {
			parts = append(parts, fmt.Sprintf("%s: %s", e.serverName(l.server), l.id))
		}
		sort.Strings(parts)
		label := e.labelOf(locs[0].server, locs[0].id)
		e.add("X1", "", "label:"+k, model.Finding{
			Subject: label,
			Related: sortedKeys(servers),
			Message: fmt.Sprintf("Folder %q has different folder IDs on different servers (%s); it was probably added separately instead of shared", label, strings.Join(parts, ", ")),
			Details: []model.Detail{detail("Folder IDs", strings.Join(parts, ", "))},
		})
	}
}

func (e *evaluator) labelOf(server, id string) string {
	if sv := e.Snapshot.Server(server); sv != nil {
		if f, ok := sv.Folders[id]; ok {
			return f.DisplayName()
		}
	}
	return id
}

// X2: the same folder ID placed in shares of different types.
func (e *evaluator) folderShareTypes() {
	if e.Structure.ShareType == nil {
		return
	}
	type placed struct {
		server, share, attrs string
	}
	byID := map[string][]placed{}
	for _, sv := range e.Snapshot.Servers {
		if !sv.Loaded {
			continue
		}
		add, exclude := e.sharesConfig(sv)
		shares := DetectShares(sv, add, exclude, e.Structure)
		st := pathx.StyleFor(sv.PathSep)
		for _, f := range sv.Folders {
			sh := ShareOf(shares, f.Path, st)
			if sh == nil {
				continue
			}
			byID[f.ID] = append(byID[f.ID], placed{sv.ID, sh.Name, attrString(sh.Attrs)})
		}
	}
	for _, id := range sortedKeys(byID) {
		ps := byID[id]
		kinds := map[string]bool{}
		servers := map[string]bool{}
		for _, p := range ps {
			kinds[p.attrs] = true
			servers[p.server] = true
		}
		if len(kinds) < 2 {
			continue
		}
		var parts []string
		for _, p := range ps {
			parts = append(parts, fmt.Sprintf("%s: %s", e.serverName(p.server), p.share))
		}
		sort.Strings(parts)
		label := e.labelOf(ps[0].server, id)
		e.add("X2", "", "folder:"+id, model.Finding{
			Subject: label,
			Related: sortedKeys(servers),
			Message: fmt.Sprintf("Folder %s is in different share types on different servers (%s)", label, strings.Join(parts, ", ")),
			Details: []model.Detail{detail("Folder ID", id), detail("Shares", strings.Join(parts, ", "))},
		})
	}
}

func attrString(a map[string]string) string {
	keys := sortedKeys(a)
	var b strings.Builder
	for _, k := range keys {
		v := a[k]
		fmt.Fprintf(&b, "%s=%s;", k, strings.ToUpper(v))
	}
	return b.String()
}

func (e *evaluator) sharesConfig(sv *model.Server) (add, exclude []string) {
	if s, ok := e.Config.ServerByKey(sv.ID); ok {
		return e.Config.SharesFor(s)
	}
	return nil, nil
}
