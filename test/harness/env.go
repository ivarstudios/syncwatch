package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Spec describes the simulated deployment.
type Spec struct {
	Dir          string // working directory (wiped on start)
	SyncthingBin string
	BasePort     int // GUI ports start here, sync ports at BasePort+1000
	Servers      []string
	PCs          []string
}

// DefaultSpec is three NAS servers and two PCs, like a small IVAR.
func DefaultSpec(dir, bin string, basePort int) Spec {
	return Spec{Dir: dir, SyncthingBin: bin, BasePort: basePort, Servers: []string{"AKKA", "SOL", "SIRIUS"}, PCs: []string{"MORTIS", "VENUS"}}
}

// Env is a running simulated deployment.
type Env struct {
	Spec      Spec
	Instances map[string]*Instance
	Order     []*Instance
	Log       func(format string, a ...any)
}

// Get returns an instance by name.
func (e *Env) Get(name string) *Instance { return e.Instances[name] }

// Start creates, configures and starts every instance and connects servers
// to each other and PCs to every server.
func Start(ctx context.Context, spec Spec, logf func(string, ...any)) (*Env, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := os.RemoveAll(spec.Dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return nil, err
	}
	e := &Env{Spec: spec, Instances: map[string]*Instance{}, Log: logf}
	n := 0
	add := func(name string, server bool) {
		inst := newInstance(spec.SyncthingBin, spec.Dir, name, server, spec.BasePort+n, spec.BasePort+1000+n)
		e.Instances[name] = inst
		e.Order = append(e.Order, inst)
		n++
	}
	for _, s := range spec.Servers {
		add(s, true)
	}
	for _, p := range spec.PCs {
		add(p, false)
	}
	for _, inst := range e.Order {
		if err := inst.generate(); err != nil {
			e.Close()
			return nil, err
		}
		if err := inst.Start(ctx); err != nil {
			e.Close()
			return nil, err
		}
		if err := inst.SetName(ctx); err != nil {
			e.Close()
			return nil, err
		}
		logf("started %s (%s) on %s, device %s", inst.Name, role(inst), inst.URL(), inst.DeviceID[:7])
	}
	for _, a := range e.Order {
		for _, b := range e.Order {
			if a == b || (!a.Server && !b.Server) {
				continue
			}
			if err := a.AddDevice(ctx, b); err != nil {
				e.Close()
				return nil, err
			}
		}
	}
	return e, nil
}

func role(i *Instance) string {
	if i.Server {
		return "server"
	}
	return "PC"
}

// Close stops every instance.
func (e *Env) Close() {
	for _, inst := range e.Order {
		_ = inst.Stop()
	}
}

// Placement puts a folder on an instance: a share for servers, a path for PCs.
type Placement struct {
	Inst  string
	Share string // servers: share directory name, e.g. AKKA-LIVE-1SOURCE
}

// AddProject creates a project folder shared between the given instances.
func (e *Env) AddProject(ctx context.Context, id, label string, places ...Placement) error {
	var with []*Instance
	for _, p := range places {
		with = append(with, e.Instances[p.Inst])
	}
	for _, p := range places {
		inst := e.Instances[p.Inst]
		path := inst.PCDir(label)
		if inst.Server {
			path = filepath.Join(inst.ShareDir(p.Share), label)
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
		if err := inst.AddFolder(ctx, id, label, path, with); err != nil {
			return err
		}
	}
	return nil
}

// Mkdir creates directories (relative to a server's share) on disk.
func (e *Env) Mkdir(inst, share string, parts ...string) (string, error) {
	p := filepath.Join(append([]string{e.Instances[inst].ShareDir(share)}, parts...)...)
	return p, os.MkdirAll(p, 0o755)
}

// WriteFile writes a file inside a server share.
func (e *Env) WriteFile(inst, share, rel, content string) error {
	p := filepath.Join(e.Instances[inst].ShareDir(share), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// BuildIVAR creates the shares and folders of the default scenario:
// healthy project folders plus the IVAR acceptance cases.
func (e *Env) BuildIVAR(ctx context.Context) error {
	shares := map[string][]string{
		"AKKA":   {"AKKA-LIVE-1SOURCE", "AKKA-LIVE-2PROJECTFILES"},
		"SOL":    {"SOL-LIVE-1SOURCE", "SOL-LIVE-2PROJECTFILES", "SOL-ME-LIVE-1SOURCE"},
		"SIRIUS": {"SIRIUS-LIVE-1SOURCE", "SIRIUS-LIVE-2PROJECTFILES"},
	}
	for srv, list := range shares {
		if e.Instances[srv] == nil {
			continue
		}
		for _, sh := range list {
			if err := os.MkdirAll(e.Instances[srv].ShareDir(sh), 0o755); err != nil {
				return err
			}
		}
	}
	steps := []struct {
		id, label string
		places    []Placement
	}{
		{"p-alpha", "LIVE-ALPHA-2PROJECTFILES", []Placement{{"AKKA", "AKKA-LIVE-2PROJECTFILES"}, {"SOL", "SOL-LIVE-2PROJECTFILES"}, {"MORTIS", ""}}},
		{"p-beta", "LIVE-BETA-1SOURCE", []Placement{{"AKKA", "AKKA-LIVE-1SOURCE"}, {"SOL", "SOL-LIVE-1SOURCE"}, {"SIRIUS", "SIRIUS-LIVE-1SOURCE"}, {"VENUS", ""}}},
		{"p-gamma", "LIVE-GAMMA-1SOURCE", []Placement{{"AKKA", "AKKA-LIVE-1SOURCE"}, {"SIRIUS", "SIRIUS-LIVE-1SOURCE"}, {"MORTIS", ""}}},
		{"p-skane", "LIVE-SKÅNE-ÄÖ-1SOURCE", []Placement{{"SOL", "SOL-LIVE-1SOURCE"}, {"VENUS", ""}}},
		{"p-delta", "ME-LIVE-DELTA-1SOURCE", []Placement{{"SOL", "SOL-ME-LIVE-1SOURCE"}, {"MORTIS", ""}}},
		{"p-epsilon", "LIVE-EPSILON-2PROJECTFILES", []Placement{{"SIRIUS", "SIRIUS-LIVE-2PROJECTFILES"}, {"SOL", "SOL-LIVE-2PROJECTFILES"}}},
	}
	for _, s := range steps {
		var places []Placement
		for _, p := range s.places {
			if e.Instances[p.Inst] != nil {
				places = append(places, p)
			}
		}
		if err := e.AddProject(ctx, s.id, s.label, places...); err != nil {
			return fmt.Errorf("folder %s: %w", s.id, err)
		}
	}
	// Some content, so folders have bytes and a last-changed file.
	for _, f := range []struct{ inst, share, rel string }{
		{"AKKA", "AKKA-LIVE-2PROJECTFILES", "LIVE-ALPHA-2PROJECTFILES/scene.blend"},
		{"AKKA", "AKKA-LIVE-1SOURCE", "LIVE-BETA-1SOURCE/footage/clip001.txt"},
		{"SOL", "SOL-LIVE-1SOURCE", "LIVE-SKÅNE-ÄÖ-1SOURCE/räksmörgås.txt"},
	} {
		if err := e.WriteFile(f.inst, f.share, f.rel, "hello "+f.rel+"\n"); err != nil {
			return err
		}
	}
	// Acceptance cases: directories that are not Syncthing folders.
	for _, d := range []struct{ inst, share, name string }{
		{"AKKA", "AKKA-LIVE-1SOURCE", "LIVE-NOTSYNCED-1SOURCE"},            // S7
		{"AKKA", "AKKA-LIVE-1SOURCE", "random stuff"},                      // S4
		{"AKKA", "AKKA-LIVE-1SOURCE", "ME-LIVE-WORKSHOP"},                  // S5 placement (+S7)
		{"AKKA", "AKKA-LIVE-1SOURCE", "LIVE-KYRGYZ-2PROJECTFILES-BLENDER"}, // S5 wrong type + S6
		{"AKKA", "AKKA-LIVE-2PROJECTFILES", "LIVE-KYRGYZ-2PROJECTFILES-BLENDER"},
		{"SOL", "SOL-LIVE-1SOURCE", "LIVE-X[1]-1SOURCE"}, // brackets (S7)
		{"SOL", "SOL-LIVE-1SOURCE", "@Recycle"},          // ignored
		{"SOL", "SOL-LIVE-1SOURCE", ".streams"},          // ignored
	} {
		if e.Instances[d.inst] == nil {
			continue
		}
		if _, err := e.Mkdir(d.inst, d.share, d.name); err != nil {
			return err
		}
	}
	return nil
}

// WaitConnected waits until every server is connected to every other device it knows.
func (e *Env) WaitConnected(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		missing := []string{}
		for _, a := range e.Order {
			if !a.Running() {
				continue
			}
			for _, b := range e.Order {
				if a == b || !b.Running() || (!a.Server && !b.Server) {
					continue
				}
				if !a.Connected(ctx, b) {
					missing = append(missing, a.Name+"→"+b.Name)
				}
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not connected: %s", strings.Join(missing, ", "))
		}
		time.Sleep(time.Second)
	}
}
