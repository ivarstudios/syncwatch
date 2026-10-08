package rules

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/pathx"
)

func (e *evaluator) structure() {
	for _, sv := range e.Snapshot.Servers {
		if !sv.Loaded {
			continue
		}
		e.overlaps(sv)
		e.shareRoots(sv)
		e.nested(sv)
		e.directories(sv)
	}
}

// S1: one Syncthing folder inside another.
func (e *evaluator) overlaps(sv *model.Server) {
	st := pathx.StyleFor(sv.PathSep)
	ids := sortedFolderIDs(sv)
	for _, a := range ids {
		for _, b := range ids {
			fa, fb := sv.Folders[a], sv.Folders[b]
			if a == b || fa.Path == "" || fb.Path == "" {
				continue
			}
			if !st.Within(fb.Path, fa.Path) {
				continue
			}
			e.add("S1", sv.ID, "overlap:"+fb.ID+":"+fa.ID, model.Finding{
				Subject: fmt.Sprintf("%s inside %s on %s", fb.DisplayName(), fa.DisplayName(), sv.Name),
				Message: fmt.Sprintf("Syncthing folder %s is inside folder %s on %s; files would be synced twice", fb.DisplayName(), fa.DisplayName(), sv.Name),
				Details: []model.Detail{
					detail("Inner folder", fb.ID+" — "+fb.Path),
					detail("Outer folder", fa.ID+" — "+fa.Path),
				},
			})
		}
	}
}

// S2: a share root that is itself a Syncthing folder.
func (e *evaluator) shareRoots(sv *model.Server) {
	st := pathx.StyleFor(sv.PathSep)
	add, exclude := e.sharesConfig(sv)
	shares := DetectShares(sv, add, exclude, e.Structure)
	for _, id := range sortedFolderIDs(sv) {
		f := sv.Folders[id]
		for _, sh := range shares {
			if !st.Equal(f.Path, sh.Path) {
				continue
			}
			e.add("S2", sv.ID, "share-root:"+f.ID, model.Finding{
				Subject: fmt.Sprintf("%s on %s", sh.Name, sv.Name),
				Message: fmt.Sprintf("Share root %s on %s is itself a Syncthing folder (%s); sync project folders individually instead", sh.Name, sv.Name, f.DisplayName()),
				Details: []model.Detail{detail("Folder ID", f.ID), detail("Path", f.Path)},
			})
			break
		}
	}
}

// S3: a project folder nested inside another project folder.
func (e *evaluator) nested(sv *model.Server) {
	if sv.Structure == nil || !e.Structure.HasProjectPattern() {
		return
	}
	st := pathx.StyleFor(sv.PathSep)
	keys := sortedKeys(sv.Structure.Nested)
	for _, k := range keys {
		n := sv.Structure.Nested[k]
		rel := st.Display(st.Rel(n.Path, n.Parent))
		parent := st.Base(n.Parent)
		share := st.Base(n.Share)
		details := []model.Detail{detail("Path", n.Path), detail("Found by", n.Source)}
		if n.FolderID != "" {
			details = append(details, detail("Syncthing folder", n.FolderID))
		}
		e.add("S3", sv.ID, "nested:"+strings.ToLower(n.Path), model.Finding{
			Subject: fmt.Sprintf("%s/%s/%s on %s", share, parent, rel, sv.Name),
			Message: fmt.Sprintf("Project folder %s is nested inside project folder %s (%s on %s)", st.Base(n.Path), parent, share, sv.Name),
			Since:   n.FoundAt,
			Details: details,
		})
	}
}

type dirReason struct {
	check string
	text  string
}

// S4–S7, combined into one finding per directory.
func (e *evaluator) directories(sv *model.Server) {
	if sv.Structure == nil {
		return
	}
	cs := e.Structure
	// S6 needs every name on the server first.
	byName := map[string][]string{}
	for _, sh := range sv.Structure.Shares {
		for _, d := range sh.Dirs {
			if cs.Ignored(d.Name) {
				continue
			}
			k := strings.ToLower(d.Name)
			byName[k] = append(byName[k], sh.Name)
		}
	}
	for _, sh := range sv.Structure.Shares {
		for _, d := range sh.Dirs {
			if cs.Ignored(d.Name) {
				continue
			}
			var reasons []dirReason
			isProject := cs.IsProject(d.Name)
			if cs.HasProjectPattern() && !isProject {
				reasons = append(reasons, dirReason{"S4", "name doesn't match the project pattern"})
			}
			if isProject {
				reasons = append(reasons, e.placement(sh, d)...)
			}
			if others := byName[strings.ToLower(d.Name)]; len(others) > 1 {
				var elsewhere []string
				for _, o := range others {
					if o != sh.Name {
						elsewhere = append(elsewhere, o)
					}
				}
				if len(elsewhere) == 0 {
					elsewhere = []string{sh.Name}
				}
				sort.Strings(elsewhere)
				reasons = append(reasons, dirReason{"S6", "same name also in " + strings.Join(elsewhere, ", ")})
			}
			if isProject && len(d.FolderIDs) == 0 {
				reasons = append(reasons, dirReason{"S7", "not covered by any Syncthing folder"})
			}
			if len(reasons) == 0 {
				continue
			}
			e.addDir(sv, sh, d, reasons)
		}
	}
}

// placement returns S5 reasons: type tokens that disagree with the share
// type, ambiguous tokens, and failed placement rules.
func (e *evaluator) placement(sh *model.Share, d *model.Dir) []dirReason {
	cs := e.Structure
	var out []dirReason
	tokens := cs.NameTokens(d.Name)
	shareType := sh.Attrs["type"]
	switch {
	case len(tokens) > 1:
		out = append(out, dirReason{"S5", "name has more than one type token (" + strings.Join(tokens, ", ") + ")"})
	case len(tokens) == 1 && shareType != "" && !strings.EqualFold(tokens[0], shareType):
		out = append(out, dirReason{"S5", fmt.Sprintf("name says %s but the share is %s", tokens[0], shareType)})
	}
	if cs.ShareType != nil {
		for _, p := range cs.Placement {
			if !p.When.MatchString(d.Name) {
				continue
			}
			for _, k := range sortedKeys(p.Must) {
				want := p.Must[k]
				if !config.AttrSatisfies(sh.Attrs[k], want) {
					out = append(out, dirReason{"S5", fmt.Sprintf("names matching %s must be in a share where %s is %s", p.Source, k, want)})
				}
			}
		}
	}
	return out
}

func (e *evaluator) addDir(sv *model.Server, sh *model.Share, d *model.Dir, reasons []dirReason) {
	var checks, texts []string
	var sev model.Severity
	primary := ""
	urgent := false
	seen := map[string]bool{}
	for _, r := range reasons {
		if !e.Config.CheckEnabled(r.check) {
			continue
		}
		texts = append(texts, r.text)
		if !seen[r.check] {
			seen[r.check] = true
			checks = append(checks, r.check)
		}
		info := config.CatalogByID[r.check]
		if info.Severity > sev {
			sev, primary = info.Severity, r.check
		}
		urgent = urgent || e.Config.Urgent(r.check)
	}
	if len(checks) == 0 {
		return
	}
	details := []model.Detail{detail("Path", d.Path), detail("Share", sh.Name)}
	if len(d.FolderIDs) > 0 {
		details = append(details, detail("Syncthing folders", strings.Join(d.FolderIDs, ", ")))
	}
	if len(sh.Attrs) > 0 {
		details = append(details, detail("Share attributes", attrDisplay(sh.Attrs)))
	}
	e.out = append(e.out, model.Finding{
		ID:       model.FindingID("DIR", sv.ID, strings.ToLower(d.Path)),
		Check:    primary,
		Checks:   checks,
		Severity: sev,
		Category: model.CatStructure,
		Server:   sv.ID,
		Subject:  fmt.Sprintf("%s/%s on %s", sh.Name, d.Name, sv.Name),
		Message:  fmt.Sprintf("%s in %s on %s: %s", d.Name, sh.Name, sv.Name, strings.Join(texts, "; ")),
		Details:  details,
		Link:     e.link(sv.ID),
		Urgent:   urgent,
	})
}

func attrDisplay(a map[string]string) string {
	var parts []string
	for _, k := range sortedKeys(a) {
		v := a[k]
		if v == "" {
			v = "—"
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ", ")
}
