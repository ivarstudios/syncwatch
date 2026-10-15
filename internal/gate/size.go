package gate

// Folder sizes. Syncthing reports the size of its own folders only. The gate
// runs on the NAS, thus it can measure the other directories of the shares
// on the file system: syncwatch shows them on its Folders page as project
// folders that are not in Syncthing yet. The gate only reads: it lists
// directories and reads the size of each file (lstat), never the contents of
// a file, and it does not follow symbolic links. The shares are mounted
// read-only in the gate container, so a fault in this code cannot change a
// file either.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/stclient"
)

const pathSize = "/syncwatch/size"

// Limits of a measurement. Variables, so that tests can change them.
var (
	sizePauseEvery = 512                   // entries between 2 pauses
	sizePause      = 5 * time.Millisecond  // a pause gives the disks of the NAS to Syncthing and the users
	sizeBudget     = 30 * time.Minute      // a measurement stops after this time: its result is then a lower limit
	sizeAgain      = time.Minute           // a directory is measured again at most once a minute
	maxSizeQueue   = 2000                  // queued measurements
	maxSizeResults = 5000                  // results kept; the oldest go first
	errSizeStop    = errors.New("stopped") // the walk ran out of time
)

type sizeJob struct{ path, root string }

// sizer measures directories one at a time and keeps the results, also in
// a file in the data directory of the gate, so that a restart of the gate
// or of syncwatch does not lose them.
type sizer struct {
	file string // "" keeps the results in memory only
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex
	results map[string]*stclient.DirSize // by path
	queue   []sizeJob
	running string
	wake    chan struct{}
}

func newSizer(file string, log *slog.Logger) *sizer {
	s := &sizer{file: file, log: log, now: time.Now, results: map[string]*stclient.DirSize{}, wake: make(chan struct{}, 1)}
	if file == "" {
		return s
	}
	b, err := os.ReadFile(file)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Warn("cannot read the saved folder sizes, the gate measures them again", "file", file, "err", err)
	default:
		var saved map[string]*stclient.DirSize
		if err := json.Unmarshal(b, &saved); err != nil {
			log.Warn("cannot read the saved folder sizes, the gate measures them again", "file", file, "err", err)
			break
		}
		for k, v := range saved {
			if v != nil {
				v.State = ""
				s.results[k] = v
			}
		}
	}
	return s
}

// state returns what the gate knows about the directory path.
func (s *sizer) state(path string) stclient.DirSize {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked(path)
}

func (s *sizer) stateLocked(path string) stclient.DirSize {
	out := stclient.DirSize{}
	if r := s.results[path]; r != nil {
		out = *r
	}
	out.Path, out.State = path, ""
	switch {
	case s.running == path:
		out.State = stclient.SizeMeasuring
	case slices.ContainsFunc(s.queue, func(j sizeJob) bool { return j.path == path }):
		out.State = stclient.SizeQueued
	}
	return out
}

// request queues a measurement of path, a directory below root, and
// returns its state. A directory that is queued or measured now, or that
// was measured less than sizeAgain ago, is not queued again.
func (s *sizer) request(path, root string) stclient.DirSize {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(path)
	if st.State != "" {
		return st
	}
	if r := s.results[path]; r != nil && !r.MeasuredAt.IsZero() && s.now().Sub(r.MeasuredAt) < sizeAgain {
		return st
	}
	if len(s.queue) >= maxSizeQueue {
		st.Error = fmt.Sprintf("the gate has %d measurements in its queue: try again later", len(s.queue))
		return st
	}
	s.queue = append(s.queue, sizeJob{path, root})
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return s.stateLocked(path)
}

// run measures the queued directories, one at a time, until ctx ends.
func (s *sizer) run(ctx context.Context) {
	for {
		s.mu.Lock()
		var job sizeJob
		ok := len(s.queue) > 0
		if ok {
			job, s.queue = s.queue[0], s.queue[1:]
			s.running = job.path
		}
		s.mu.Unlock()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		res := measure(ctx, job.root, job.path, s.now)
		s.mu.Lock()
		s.running = ""
		if ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		s.results[job.path] = &res
		s.pruneLocked()
		data, err := json.Marshal(s.results)
		s.mu.Unlock()
		if err == nil {
			s.save(data)
		}
		if res.Error != "" {
			s.log.Warn("cannot measure the folder size", "path", job.path, "err", res.Error)
			continue
		}
		// Info level: the operator compares what a measurement costs the NAS.
		s.log.Info("folder size measured", "path", job.path, "gb", fmt.Sprintf("%.1f", float64(res.Bytes)/1e9), "files", res.Files, "dirs", res.Dirs,
			"skipped", res.Skipped, "complete", res.Complete, "took", (time.Duration(res.TookMS) * time.Millisecond).String())
	}
}

// pruneLocked keeps the newest maxSizeResults results.
func (s *sizer) pruneLocked() {
	if len(s.results) <= maxSizeResults {
		return
	}
	paths := make([]string, 0, len(s.results))
	for p := range s.results {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(a, b int) bool { return s.results[paths[a]].MeasuredAt.Before(s.results[paths[b]].MeasuredAt) })
	for _, p := range paths[:len(paths)-maxSizeResults] {
		delete(s.results, p)
	}
}

// save writes the results to the file: a new file, then a rename, so that
// a stop of the gate cannot leave half a file. The file has only paths and
// numbers; it is private like the rest of the data directory.
func (s *sizer) save(data []byte) {
	if s.file == "" {
		return
	}
	tmp := s.file + ".new"
	err := os.WriteFile(tmp, data, 0o600)
	if err == nil {
		_ = os.Chmod(tmp, 0o600) // a share with inherited ACLs can make a new file 0777
		err = os.Rename(tmp, s.file)
	}
	if err != nil {
		s.log.Warn("cannot save the folder sizes", "file", s.file, "err", err)
	}
}

// measure walks dir, a directory below the share root, and adds the sizes
// of its files. It reads names and sizes only, and it does not follow
// symbolic links: none may be on the way from root to dir either, so that
// the walk stays in the share.
func measure(ctx context.Context, root, dir string, now func() time.Time) (res stclient.DirSize) {
	start := now()
	res.Path = dir
	defer func() {
		res.MeasuredAt = now()
		res.TookMS = res.MeasuredAt.Sub(start).Milliseconds()
	}()
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		res.Error = "the gate cannot see the share " + root + ": mount it read-only in the gate container"
		return res
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		res.Error = "the directory is not inside the share " + root
		return res
	}
	p := root
	for _, e := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, e)
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.Error = "the directory does not exist"
			return res
		case err != nil:
			res.Error = "cannot read the directory: " + err.Error()
			return res
		case fi.Mode()&fs.ModeSymlink != 0:
			res.Error = "the path has a symbolic link (" + p + "): the gate does not follow it"
			return res
		case p == filepath.Clean(dir) && !fi.IsDir():
			res.Error = "not a directory"
			return res
		}
	}

	var n int64
	stopped := false
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			res.Skipped++ // a directory that cannot be read: the result is a lower limit
			return nil
		}
		if n++; n%int64(sizePauseEvery) == 0 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if now().Sub(start) > sizeBudget {
				stopped = true
				return errSizeStop
			}
			time.Sleep(sizePause)
		}
		switch {
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				res.Skipped++
				return nil
			}
			res.Bytes += info.Size()
			res.Files++
		case d.IsDir() && p != dir:
			res.Dirs++
		}
		return nil
	})
	res.Complete = err == nil && !stopped && res.Skipped == 0
	return res
}

// checkSize cleans the directory of a size request and reports whether the
// token may measure it: it has to lie inside, not at, a directory that a
// browse request may list (a share). It returns the cleaned path and the
// nearest such root. err means Syncthing's folder list isn't known yet.
func (g *Gate) checkSize(path string) (dir, root string, ok bool, err error) {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	if err := g.refreshRootsLocked(rootsMaxAge); err != nil && g.rootsAt.IsZero() {
		return "", "", false, err
	}
	p := parseBrowsePath(path, g.windows)
	p.dir = false
	if p.root == "" || len(p.elems) == 0 {
		return "", "", false, nil
	}
	r, ok := g.sizeRootLocked(p)
	if !ok {
		// A folder may just have been added: look again, at most once a minute.
		if g.refreshRootsLocked(rootsMissAge) != nil {
			return "", "", false, nil
		}
		if r, ok = g.sizeRootLocked(p); !ok {
			return "", "", false, nil
		}
	}
	r.dir = false
	return p.String(g.windows), r.String(g.windows), true, nil
}

// sizeRootLocked returns the nearest root that p lies inside.
func (g *Gate) sizeRootLocked(p browsePath) (browsePath, bool) {
	var best browsePath
	found := false
	for _, r := range g.roots {
		if len(p.elems) > len(r.elems) && p.under(r, g.windows) && (!found || len(r.elems) > len(best.elems)) {
			best, found = r, true
		}
	}
	return best, found
}

// serveSize answers /syncwatch/size: what the gate knows about the size of
// a directory and, with measure=1, a new measurement in its queue.
func (g *Gate) serveSize(w http.ResponseWriter, r *http.Request, q url.Values) {
	dir, root, ok, err := g.checkSize(q.Get("path"))
	if err != nil {
		http.Error(w, upstreamError(err, "cannot read the folder list of Syncthing"), http.StatusBadGateway)
		return
	}
	if !ok {
		g.deny(w, r, http.StatusForbidden, "directory not in a permitted browse root", "")
		return
	}
	var res stclient.DirSize
	if q.Get("measure") == "1" {
		res = g.sizes.request(dir, root)
	} else {
		res = g.sizes.state(dir)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// Run measures the folder sizes that syncwatch asks for, until ctx ends.
func (g *Gate) Run(ctx context.Context) { g.sizes.run(ctx) }
