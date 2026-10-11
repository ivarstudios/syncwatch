package rules

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// What H5 shows of a sync that does not finish: the files that a device
// still needs (W-058), and the progress of a sync that continues: the bytes
// received, the speed and the time left (W-059). The time rule of H5
// (stuck_sync) does not change. Sizes have 2 significant digits and the
// time left whole minutes, hours or days, so that the message and the rows
// do not change at each evaluation.

// speedWindow is the time over which H5 measures the speed of a sync.
const speedWindow = 10 * time.Minute

// syncProgress is the progress of a sync as text; "" when not known.
type syncProgress struct {
	received string // "1.6 of 2.1 GB"
	speed    string // "2.4 MB each second", or "0 (no data arrived in the last 10 minutes)"
	left     string // "approximately 4 minutes"; "" when the speed is 0 or not known
}

// details gives the rows Received, Speed and Time left that have a value.
func (p syncProgress) details() []model.Detail {
	var out []model.Detail
	for _, d := range []model.Detail{{Key: "Received", Value: p.received}, {Key: "Speed", Value: p.speed}, {Key: "Time left", Value: p.left}} {
		if d.Value != "" {
			out = append(out, d)
		}
	}
	return out
}

// suffix gives the part of the H5 message: ", 1.6 of 2.1 GB received,
// approximately 4 minutes remaining". The message of a device has its
// percentage already: without received, it stays at 25 words or less.
func (p syncProgress) suffix(received bool) string {
	var s string
	if received && p.received != "" {
		s += ", " + p.received + " received"
	}
	if p.left != "" {
		s += ", " + p.left + " remaining"
	}
	return s
}

// progressOf gives the progress of a folder for a device or for the
// server's own copy: need and global are the bytes still needed and the
// size of the folder (0 when not known); samples are the progress samples
// of the pair (none for a device that is away).
func (e *evaluator) progressOf(samples []model.ProgressSample, need, global int64) syncProgress {
	var p syncProgress
	if global > 0 {
		p.received = sizeOf(max(global-need, 0), global)
	}
	bps, ok := syncSpeed(samples, need, e.Now)
	switch {
	case !ok:
	case bps <= 0:
		p.speed = "0 (no data arrived in the last 10 minutes)"
	default:
		p.speed = rate(bps)
		if need > 0 {
			p.left = timeLeft(float64(need) / bps)
		}
	}
	return p
}

// syncSpeed gives the bytes that arrived each second: the decrease of the
// need between the newest point and the newest sample that is at least 10
// minutes older. The newest point is the newest sample, or the need now when
// no sample came in the last minute (while a sync waits, Syncthing sends no
// progress). ok is false when the samples do not go back 10 minutes.
func syncSpeed(samples []model.ProgressSample, need int64, now time.Time) (bps float64, ok bool) {
	if len(samples) == 0 {
		return 0, false
	}
	end := samples[len(samples)-1]
	if now.Sub(end.At) >= time.Minute {
		end = model.ProgressSample{At: now, NeedBytes: need}
	}
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		d := end.At.Sub(s.At)
		if d < speedWindow {
			continue
		}
		if s.NeedBytes <= end.NeedBytes {
			return 0, true
		}
		return float64(s.NeedBytes-end.NeedBytes) / d.Seconds(), true
	}
	return 0, false
}

// sizeOf writes a part of a size in the unit of the size: "1.6 of 2.1 GB".
// 1 GB is 1,000,000,000 bytes.
func sizeOf(part, total int64) string {
	unit, name := 1e6, "MB"
	if total >= 1e9 {
		unit, name = 1e9, "GB"
	}
	return sig2(float64(part)/unit) + " of " + sig2(float64(total)/unit) + " " + name
}

// rate writes a speed: "2.4 MB each second".
func rate(bps float64) string {
	if bps >= 1e9 {
		return sig2(bps/1e9) + " GB each second"
	}
	return sig2(bps/1e6) + " MB each second"
}

// timeLeft writes a time in seconds in whole minutes (less than 2 hours),
// hours (less than 2 days) or days: "approximately 4 minutes".
func timeLeft(secs float64) string {
	switch {
	case secs < 30:
		return "less than 1 minute"
	case secs < 2*3600:
		return "approximately " + Plural(int(math.Round(secs/60)), "minute", "minutes")
	case secs < 48*3600:
		return "approximately " + Plural(int(math.Round(secs/3600)), "hour", "hours")
	}
	return "approximately " + Plural(int(math.Round(secs/86400)), "day", "days")
}

// sig2 writes a number with 2 significant digits and no trailing zeros:
// 2.4, 0.59, 12, 590, 1200.
func sig2(v float64) string {
	if v <= 0 {
		return "0"
	}
	scale := math.Pow(10, math.Floor(math.Log10(v))-1)
	r := math.Round(v/scale) * scale
	dec := max(int(1-math.Floor(math.Log10(r))), 0)
	s := strconv.FormatFloat(r, 'f', dec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

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
