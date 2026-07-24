package activity

import "testing"

func TestRecorder_RecentAndErrorResolve(t *testing.T) {
	r := New()
	r.Add(Event{Local: "/l", Path: "a.txt", Kind: "upload"})
	r.Add(Event{Local: "/l", Path: "b.txt", Kind: "download", Err: "boom"})

	recent := r.Recent()
	if len(recent) != 2 {
		t.Fatalf("Recent len = %d, want 2", len(recent))
	}
	if recent[0].Path != "b.txt" { // newest first
		t.Errorf("Recent[0] = %q, want b.txt", recent[0].Path)
	}

	if errs := r.Errors(); len(errs) != 1 || errs[0].Path != "b.txt" {
		t.Fatalf("Errors = %+v, want one for b.txt", errs)
	}

	// A later success for b.txt resolves the error.
	r.Add(Event{Local: "/l", Path: "b.txt", Kind: "download"})
	if errs := r.Errors(); len(errs) != 0 {
		t.Errorf("Errors after resolve = %+v, want empty", errs)
	}
}

func TestRecorder_Subscribe(t *testing.T) {
	r := New()
	ch := r.Subscribe()
	r.Add(Event{Local: "/l", Path: "a.txt", Kind: "upload"})
	select {
	case e := <-ch:
		if e.Path != "a.txt" {
			t.Errorf("got %q", e.Path)
		}
	default:
		t.Fatal("subscriber received nothing")
	}
}

func TestRecorder_RingBuffer(t *testing.T) {
	r := New()
	for i := 0; i < maxEvents+50; i++ {
		r.Add(Event{Local: "/l", Path: "f", Kind: "upload"})
	}
	if got := len(r.Recent()); got != maxEvents {
		t.Errorf("Recent len = %d, want %d (capped)", got, maxEvents)
	}
}
