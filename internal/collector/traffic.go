package collector

import (
	"context"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// trafficEvery is how often a server's traffic counters are read: one
// cheap /rest/system/connections call, for the bytes moved per day and week
// and the current transfer rate.
var trafficEvery = time.Minute

// trafficLoop reads the traffic counters while the collector runs.
func (c *Collector) trafficLoop(ctx context.Context) {
	t := time.NewTicker(c.trafficEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.sampleTraffic(ctx)
	}
}

// sampleTraffic records one reading. Errors are left to the session, which
// reports a server that can't be reached.
func (c *Collector) sampleTraffic(ctx context.Context) {
	var start time.Time
	var up bool
	c.hub.View(c.id, func(s *model.Server) { start, up = s.StartTime, s.Status == model.StatusUp && s.Loaded })
	if !up {
		c.update(func(s *model.Server) { s.Rate = model.Rate{} })
		return
	}
	tr, err := c.client.Traffic(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	sm, err := c.st.RecordTraffic(c.id, start.UTC().Format(time.RFC3339), tr.InBytesTotal, tr.OutBytesTotal, now)
	if err != nil {
		c.log.Warn("recording traffic", "err", err)
		return
	}
	if secs := sm.Over.Seconds(); secs > 0 {
		c.update(func(s *model.Server) {
			s.Rate = model.Rate{In: float64(sm.In) / secs, Out: float64(sm.Out) / secs, At: now}
		})
	}
}
