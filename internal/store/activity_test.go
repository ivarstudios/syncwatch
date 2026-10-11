package store

import (
	"path/filepath"
	"testing"
	"time"
)

// The first-seen time of a folder stays as it was first recorded; the last
// local change only moves forward; a local change of a folder without a row
// makes the row. The rows survive a restart.
func TestFolderActivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	key := make([]byte, 32)
	s, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 1, 14, 5, 0, 0, time.UTC)
	if err := s.NoteFoldersSeen("akka", []string{"f1", "f2"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteFoldersSeen("akka", []string{"f1", "f3"}, t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteFoldersSeen("sol", []string{"f1"}, t0.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteFoldersSeen("akka", nil, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastLocalChange("akka", "f1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastLocalChange("akka", "f1", t0.Add(time.Hour)); err != nil { // older: no change
		t.Fatal(err)
	}
	if err := s.SetLastLocalChange("akka", "f9", t0.Add(5*time.Hour)); err != nil { // no row yet
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.FolderActivity("akka")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]FolderActivity{
		"f1": {FirstSeen: t0, LastLocalChange: t0.Add(2 * time.Hour)},
		"f2": {FirstSeen: t0},
		"f3": {FirstSeen: t0.Add(24 * time.Hour)},
		"f9": {FirstSeen: t0.Add(5 * time.Hour), LastLocalChange: t0.Add(5 * time.Hour)},
	}
	if len(got) != len(want) {
		t.Fatalf("akka: %+v", got)
	}
	for id, w := range want {
		if g := got[id]; !g.FirstSeen.Equal(w.FirstSeen) || !g.LastLocalChange.Equal(w.LastLocalChange) {
			t.Errorf("%s: %+v, want %+v", id, g, w)
		}
	}
	if err := s.SetLastLocalChange("akka", "f1", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.FolderActivity("akka"); !got["f1"].LastLocalChange.Equal(t0.Add(3*time.Hour)) || !got["f1"].FirstSeen.Equal(t0) {
		t.Fatalf("f1 after a newer change: %+v", got["f1"])
	}
	if sol, _ := s.FolderActivity("sol"); len(sol) != 1 || !sol["f1"].FirstSeen.Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("sol: %+v", sol)
	}
	if none, err := s.FolderActivity("none"); err != nil || len(none) != 0 {
		t.Fatalf("a server without rows: %+v %v", none, err)
	}
}
