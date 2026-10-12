package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// Settle waits for an evaluation that starts after the call. The
// view after it has the state of the hub at the call, for example the
// result of a structure scan that an administrator started.
func TestSettleWaitsForAnEvaluationAfterTheCall(t *testing.T) {
	r := newRig(t)
	// Without Run nothing evaluates: Settle stops after its time.
	start := time.Now()
	if r.eng.Settle(context.Background(), 100*time.Millisecond) {
		t.Fatal("Settle reported an evaluation, but nothing evaluates")
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("Settle stopped after %v", waited)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.eng.Run(ctx)
	}()
	stop := func() { cancel(); <-done } // a second call does nothing
	defer stop()
	// The first evaluation of Run is complete before the change.
	deadline := time.Now().Add(5 * time.Second)
	for r.eng.View().Evaluated.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("Run did not evaluate")
		}
		time.Sleep(10 * time.Millisecond)
	}

	r.hub.Update("a", func(s *model.Server) {
		s.Folders["f"] = &model.Folder{ID: "f", Label: "LIVE-F", Path: "/s/X/LIVE-F", State: "error", Error: "boom", Completion: map[string]*model.Completion{}}
	})
	start = time.Now()
	if !r.eng.Settle(ctx, 5*time.Second) {
		t.Fatal("no evaluation after the change")
	}
	// The Trigger of Settle ends the wait of 1 second that coalesces the
	// changes of the hub.
	if waited := time.Since(start); waited > 900*time.Millisecond {
		t.Fatalf("Settle waited %v: the 1-second wait for more changes did not end", waited)
	}
	if find(r.eng.View(), "H4", "LIVE-F") == nil {
		t.Fatal("the evaluation that Settle waited for does not have the change of the hub")
	}

	// A context that ends stops the wait at once.
	stop()
	start = time.Now()
	if r.eng.Settle(ctx, 5*time.Second) {
		t.Fatal("Settle reported an evaluation after Run stopped")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("Settle waited %v after its context ended", waited)
	}
}
