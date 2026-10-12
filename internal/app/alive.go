package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/ivarstudios/syncwatch/internal/store"
)

// syncwatch records that it runs (the heartbeat, app.alive_at): at the
// start, every aliveEvery and at a clean stop. At the next start, a
// heartbeat older than gapAfter gives a gap: a time when syncwatch did not
// run. The next notification names it (notify).
const (
	aliveEvery = 30 * time.Second
	gapAfter   = 2 * time.Minute // a restart or an update takes less
	// In the first clockCheckFor after the start, the heartbeat looks for a
	// step of the clock (clockCheck) every clockCheckEvery: NTP sets the
	// clock in the first minutes after a boot, and the messages that are
	// due go out some seconds after the step.
	clockCheckEvery = time.Second
	clockCheckFor   = 10 * time.Minute
)

// noteGap records the gap from the last heartbeat to start, when it is
// longer than gapAfter. After a crash the gap starts at the last heartbeat,
// at most aliveEvery before the crash. It logs one INFO line for the gap.
// It returns the last heartbeat (zero when there was none) for clockCheck.
func noteGap(st *store.Store, start time.Time, loc *time.Location, log *slog.Logger) (time.Time, error) {
	alive, err := st.AliveAt()
	if err != nil {
		return time.Time{}, err
	}
	return alive, addGap(st, alive, start, loc, log)
}

// addGap records the gap from alive to start when it is longer than
// gapAfter, with one INFO line that has attrs at the end.
func addGap(st *store.Store, alive, start time.Time, loc *time.Location, log *slog.Logger, attrs ...any) error {
	if alive.IsZero() || start.Sub(alive) <= gapAfter {
		return nil
	}
	if err := st.AddGap(store.Gap{From: alive, To: start}); err != nil {
		return err
	}
	log.Info("syncwatch did not run", append([]any{"from", alive.In(loc), "to", start.In(loc), "duration", start.Sub(alive).Round(time.Second).String()}, attrs...)...)
	return nil
}

// clockCheck finds a clock that was behind at the start. A host without a
// battery clock (for example a Raspberry Pi) starts with the time of its
// last shutdown, and NTP sets the correct time some seconds or minutes
// later, often after Docker started syncwatch. noteGap then compared the
// last heartbeat with the wrong time: it recorded no gap, or a gap that
// ends too early. The monotonic clock of the process is correct from the
// start. When the clock moved forward by more than gapAfter in addition to
// the time that passed, the start was later than the clock said: the gap
// goes from the last heartbeat to the corrected time of the start.
type clockCheck struct {
	st      *store.Store
	log     *slog.Logger
	loc     *time.Location
	prev    time.Time // the last heartbeat before the start; zero: none
	started time.Time // the clock at the start, with the monotonic clock
	done    bool
}

// check compares the clock now with elapsed, the time after the start that
// the monotonic clock measured. It corrects the gap one time, in the first
// clockCheckFor. A gap that noteGap recorded from the same heartbeat is
// replaced (store.AddGap), thus the next message names the corrected gap.
func (c *clockCheck) check(now time.Time, elapsed time.Duration) {
	if c.done {
		return
	}
	if c.prev.IsZero() || elapsed > clockCheckFor {
		c.done = true
		return
	}
	now = now.Round(0) // the clock, not the monotonic clock
	behind := now.Sub(c.started.Round(0)) - elapsed
	if behind <= gapAfter {
		return
	}
	c.done = true
	if err := addGap(c.st, c.prev, now.Add(-elapsed), c.loc, c.log, "clock_behind", behind.Round(time.Second).String()); err != nil {
		c.log.Warn("cannot record the time when syncwatch did not run", "err", err)
	}
}

// heartbeat writes the heartbeat every aliveEvery until ctx ends. A write
// that fails is logged one time, until a write works again. In the first
// clockCheckFor it also checks the clock every clockCheckEvery (clockCheck).
func heartbeat(st *store.Store, log *slog.Logger, clock *clockCheck) func(context.Context) {
	return func(ctx context.Context) {
		t := time.NewTicker(aliveEvery)
		defer t.Stop()
		c := time.NewTicker(clockCheckEvery)
		defer c.Stop()
		failing := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.C:
				now := time.Now()
				clock.check(now, now.Sub(clock.started))
				if clock.done {
					c.Stop()
				}
			case <-t.C:
				switch err := st.SetAliveAt(time.Now()); {
				case err != nil && !failing:
					failing = true
					log.Warn("cannot record the heartbeat", "err", err)
				case err == nil && failing:
					failing = false
					log.Info("heartbeat recorded again")
				}
			}
		}
	}
}
