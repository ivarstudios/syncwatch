package rules

import (
	"fmt"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// What H5 shows of a sync that does not finish: the files that a device
// still needs (W-058).

// neededFiles writes the files that a device still needs (Server.NeedLists):
// the names, then "…and 15 more" when the device needed more items when the
// list was read, and the time of the list when it is older than the
// completion.
func (e *evaluator) neededFiles(nl model.NeedList, c model.Completion) string {
	parts := append([]string(nil), nl.Names...)
	if more := nl.Items - int64(len(nl.Names)); more > 0 {
		parts = append(parts, fmt.Sprintf("…and %d more", more))
	}
	s := strings.Join(parts, " · ")
	if !nl.At.IsZero() && nl.At.Before(c.UpdatedAt) {
		s += " (list from " + e.clock(nl.At) + ")"
	}
	return s
}

// clock writes a time in the configured zone: "14:05", with the date when it
// is not on the day of the evaluation ("2026-10-01 14:05").
func (e *evaluator) clock(t time.Time) string {
	loc := e.Config.Location()
	lt, ln := t.In(loc), e.Now.In(loc)
	if lt.Year() == ln.Year() && lt.YearDay() == ln.YearDay() {
		return lt.Format("15:04")
	}
	return lt.Format("2006-01-02 15:04")
}
