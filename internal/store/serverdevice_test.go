package store

import "testing"

func TestServerDeviceID(t *testing.T) {
	s := testStore(t)
	if id, err := s.ServerDeviceID("akka"); err != nil || id != "" {
		t.Fatalf("unset: %q %v", id, err)
	}
	const dev = "AKKAAAA-1111111-1111111-1111111-1111111-1111111-1111111-1111111"
	if err := s.SetServerDeviceID("akka", dev); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.ServerDeviceID("akka"); id != dev {
		t.Fatalf("got %q", id)
	}
	if id, _ := s.ServerDeviceID("sol"); id != "" {
		t.Fatalf("other server: %q", id)
	}
	if err := s.SetServerDeviceID("akka", ""); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.ServerDeviceID("akka"); id != "" {
		t.Fatalf("not removed: %q", id)
	}
}
