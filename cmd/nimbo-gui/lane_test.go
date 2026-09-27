package main

import (
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/agent"
)

func TestSetAsideUntilCoversEveryChoice(t *testing.T) {
	morning := time.Date(2026, 9, 27, 7, 30, 0, 0, time.Local)
	evening := time.Date(2026, 9, 27, 21, 0, 0, 0, time.Local)
	for _, c := range []struct {
		name    string
		now     time.Time
		minutes int
		want    time.Time
	}{
		{"1 hour", evening, 60, evening.Add(time.Hour)},
		{"4 hours", evening, 240, evening.Add(4 * time.Hour)},
		{"until tomorrow, evening", evening, -1, time.Date(2026, 9, 28, 8, 0, 0, 0, time.Local)},
		{"until tomorrow, before 8", morning, -1, time.Date(2026, 9, 27, 8, 0, 0, 0, time.Local)},
		{"until resumed", evening, 0, time.Time{}},
	} {
		if got := setAsideUntil(c.now, c.minutes); !got.Equal(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestToLaneDTO(t *testing.T) {
	until := time.Date(2026, 9, 27, 15, 30, 0, 0, time.UTC)
	got := toLaneDTO("adam", agent.LaneEntry{Path: "D/big.bin", Abs: `C:\Sync\D\big.bin`, Upload: true,
		Size: 400, Sent: 100, State: "setaside", Until: until})
	if got.Account != "adam" || got.Path != "D/big.bin" || got.Abs != `C:\Sync\D\big.bin` || got.Dir != "up" ||
		got.Size != 400 || got.DoneBytes != 100 || got.State != "setaside" || got.Until != "2026-09-27T15:30:00Z" {
		t.Errorf("got %+v", got)
	}
	if d := toLaneDTO("", agent.LaneEntry{State: "waiting", Position: 2}); d.Dir != "down" || d.Until != "" || d.Position != 2 {
		t.Errorf("download / no until: got %+v", d)
	}
}

func TestLaneActionsOnAnUnknownPathDoNothing(t *testing.T) {
	a := &App{} // no engine: every action is a no-op, never a panic
	a.LaneSyncFirst(`C:\nope`)
	a.LaneSetAside(`C:\nope`, 60)
	a.LaneResume(`C:\nope`)
	if got := a.LaneList(); len(got) != 0 {
		t.Fatalf("LaneList with no engine = %v", got)
	}
}
