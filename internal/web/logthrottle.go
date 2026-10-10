package web

import (
	"sync"
	"time"
	"unicode/utf8"
)

const (
	logEvery   = time.Minute // one log line per client, reason and minute about refused requests
	maxLogKeys = 4096        // counters kept (client × reason)
)

// logLimiter keeps repeated log lines about refused requests short: one
// line per client, reason and minute, with the number of requests since
// the last line, so a client cannot fill the log. The gate does the same.
type logLimiter struct {
	mu     sync.Mutex
	counts map[string]*logCount
}

// logCount counts one client's events for one reason since the last log
// line about them.
type logCount struct {
	n  int
	at time.Time // when they were last logged
}

func newLogLimiter() *logLimiter { return &logLimiter{counts: map[string]*logCount{}} }

// count records one event for key (client and reason). It returns the
// number of events to log now, this one included, or 0 when the line must
// wait.
func (l *logLimiter) count(key string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.counts) >= maxLogKeys {
		for k, c := range l.counts {
			if now.Sub(c.at) >= logEvery {
				delete(l.counts, k)
			}
		}
		if len(l.counts) >= maxLogKeys {
			clear(l.counts)
		}
	}
	c := l.counts[key]
	if c == nil {
		c = &logCount{}
		l.counts[key] = c
	}
	c.n++
	if !c.at.IsZero() && now.Sub(c.at) < logEvery {
		return 0
	}
	n := c.n
	c.n, c.at = 0, now
	return n
}

// cutText cuts s to at most n characters for a log line, with "…" at the end
// when it is longer.
func cutText(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for k := range s {
		if i == n {
			return s[:k] + "…"
		}
		i++
	}
	return s
}
