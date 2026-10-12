// Package rules turns a snapshot into findings. Every function here is pure:
// the same snapshot, configuration and time give the same findings.
package rules

import (
	"fmt"
	"sort"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// Input is everything the rules look at.
type Input struct {
	Snapshot  *model.Snapshot
	Config    *config.Config
	Structure *config.CompiledStructure
	Now       time.Time
	// Conflicts is the number of new sync conflicts per server ID and folder
	// ID within the conflict window.
	Conflicts map[string]map[string]int64
	// Hidden are the IDs of the findings that are snoozed or ignored now.
	Hidden map[string]bool
}

type evaluator struct {
	Input
	out  []model.Finding
	soft map[string]bool // finding IDs that stay non-urgent whatever the check's setting
}

// Evaluate runs every enabled check.
func Evaluate(in Input) []model.Finding {
	if in.Structure == nil {
		cs, err := in.Config.Structure.Compile()
		if err != nil {
			cs, _ = config.Structure{}.Compile()
		}
		in.Structure = cs
	}
	e := &evaluator{Input: in, soft: map[string]bool{}}
	e.health()
	e.cross()
	e.structure()
	e.cleanup()

	var out []model.Finding
	for _, f := range e.out {
		checks := f.Checks
		if len(checks) == 0 {
			checks = []string{f.Check}
		}
		var enabled []string
		for _, c := range checks {
			if in.Config.CheckEnabled(c) {
				enabled = append(enabled, c)
			}
		}
		if len(enabled) == 0 {
			continue
		}
		f.Checks = enabled
		if !in.Config.CheckEnabled(f.Check) {
			f.Check = enabled[0]
			f.Severity = config.CatalogByID[f.Check].Severity
		}
		for _, c := range enabled {
			if in.Config.Urgent(c) && !e.soft[f.ID] {
				f.Urgent = true
			}
		}
		if f.Category == "" {
			f.Category = config.CatalogByID[f.Check].Category
		}
		out = append(out, f)
	}
	SortFindings(out)
	return out
}

// SortFindings orders by severity (highest first), then check, server and subject.
func SortFindings(fs []model.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if a.Check != b.Check {
			return checkOrder(a.Check) < checkOrder(b.Check)
		}
		if a.Server != b.Server {
			return a.Server < b.Server
		}
		return a.Subject < b.Subject
	})
}

func checkOrder(id string) int {
	for i, c := range config.Catalog {
		if c.ID == id {
			return i
		}
	}
	return len(config.Catalog)
}

// add appends a finding for a single check and returns its ID. The check's
// catalog severity applies unless the finding sets its own.
func (e *evaluator) add(check, server, key string, f model.Finding) string {
	info := config.CatalogByID[check]
	f.Check = check
	f.Checks = []string{check}
	if f.Severity == 0 {
		f.Severity = info.Severity
	}
	f.Category = info.Category
	f.Server = server
	f.ID = model.FindingID(check, server, key)
	if f.Link == "" && server != "" {
		f.Link = e.link(server)
	}
	e.out = append(e.out, f)
	return f.ID
}

func (e *evaluator) link(serverID string) string {
	if s, ok := e.Config.ServerByKey(serverID); ok {
		return s.LinkURL()
	}
	return ""
}

func (e *evaluator) serverName(id string) string {
	if sv := e.Snapshot.Server(id); sv != nil {
		return sv.Name
	}
	return id
}

func (e *evaluator) devName(id string) string { return e.Snapshot.DeviceName(id) }

func detail(k, v string) model.Detail { return model.Detail{Key: k, Value: v} }

func timeDetail(k string, t time.Time) model.Detail {
	if t.IsZero() {
		return detail(k, "never")
	}
	return detail(k, t.UTC().Format(time.RFC3339))
}

// Ago formats a duration roughly, e.g. "3 days", "5 hours", "12 minutes".
func Ago(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
}

// Plural writes a count with its noun: Plural(1, "failed item", "failed
// items") is "1 failed item", Plural(3, ...) is "3 failed items".
func Plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
