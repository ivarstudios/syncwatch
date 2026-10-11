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
)

// noteGap records the gap from the last heartbeat to start, when it is
// longer than gapAfter. After a crash the gap starts at the last heartbeat,
// at most aliveEvery before the crash. It logs one INFO line for the gap.
func noteGap(st *store.Store, start time.Time, loc *time.Location, log *slog.Logger) error {
	alive, err := st.AliveAt()
	if err != nil || alive.IsZero() || start.Sub(alive) <= gapAfter {
		return err
	}
	if err := st.AddGap(store.Gap{From: alive, To: start}); err != nil {
		return err
	}
	log.Info("syncwatch did not run", "from", alive.In(loc), "to", start.In(loc), "duration", start.Sub(alive).Round(time.Second).String())
	return nil
}

// heartbeat writes the heartbeat every aliveEvery until ctx ends. A write
// that fails is logged one time, until a write works again.
func heartbeat(st *store.Store, log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		t := time.NewTicker(aliveEvery)
		defer t.Stop()
		failing := false
		for {
			select {
			case <-ctx.Done():
				return
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
