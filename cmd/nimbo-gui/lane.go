package main

import (
	"time"

	"github.com/otherworld/nimbo/internal/agent"
)

// The large-file queue (Deck #702, stage 2): files of 64 MB or more sync in a
// lane of their own, two at a time per account. The Status window's "Large
// files" tab lists them, and the user can move one to the front or set one
// aside for a while.

// LaneEntryDTO is one large transfer for the Status window and the flyout.
type LaneEntryDTO struct {
	Account   string `json:"account"`   // set when several accounts are signed in
	Path      string `json:"path"`      // relative to its sync folder, slash-separated
	Abs       string `json:"abs"`       // local path; what the Lane* methods take
	Dir       string `json:"dir"`       // "up" or "down"
	Size      int64  `json:"size"`
	DoneBytes int64  `json:"doneBytes"` // moved by the current attempt
	State     string `json:"state"`     // "running", "waiting", "paused" or "setaside"
	Position  int    `json:"position"`  // 1-based place in the queue, for waiting ones
	Until     string `json:"until"`     // set aside until (RFC 3339); "" = until resumed
}

func toLaneDTO(account string, en agent.LaneEntry) LaneEntryDTO {
	d := LaneEntryDTO{Account: account, Path: en.Path, Abs: en.Abs, Dir: "down", Size: en.Size,
		DoneBytes: en.Sent, State: en.State, Position: en.Position}
	if en.Upload {
		d.Dir = "up"
	}
	if !en.Until.IsZero() {
		d.Until = en.Until.Format(time.RFC3339)
	}
	return d
}

// tomorrowMorning is 08:00 on the next morning: today's if it is still
// before 8, tomorrow's otherwise. "Until tomorrow" means this for both Pause
// and Set aside.
func tomorrowMorning(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), 8, 0, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

// setAsideUntil turns the Set aside menu's choice into a time: minutes from
// now, -1 for until tomorrow, 0 for until resumed (the zero time).
func setAsideUntil(now time.Time, minutes int) time.Time {
	switch {
	case minutes < 0:
		return tomorrowMorning(now)
	case minutes == 0:
		return time.Time{}
	}
	return now.Add(time.Duration(minutes) * time.Minute)
}

// LaneList returns every account's large transfers: running, then waiting in
// queue order, then set aside.
func (a *App) LaneList() []LaneEntryDTO {
	out := []LaneEntryDTO{}
	multi := len(a.secondaries) > 0
	a.eachEngine(func(e *agent.Engine) {
		acct := ""
		if multi {
			acct = e.Account.LoginName
		}
		for _, en := range e.LaneEntries() {
			out = append(out, toLaneDTO(acct, en))
		}
	})
	return out
}

// firstEngine runs f on each engine in turn until one reports it held abs;
// the engines after that are still visited, but f is not called for them.
func (a *App) firstEngine(f func(*agent.Engine) bool) {
	done := false
	a.eachEngine(func(e *agent.Engine) {
		if !done {
			done = f(e)
		}
	})
}

// LaneSyncFirst moves a waiting large file to the front of its queue.
func (a *App) LaneSyncFirst(abs string) {
	a.firstEngine(func(e *agent.Engine) bool { return e.LaneSyncFirst(abs) })
}

// LaneSetAside sets a large file aside: minutes from now, -1 until tomorrow
// morning, 0 until the user resumes it. A restart ends it either way.
func (a *App) LaneSetAside(abs string, minutes int) {
	until := setAsideUntil(time.Now(), minutes)
	a.firstEngine(func(e *agent.Engine) bool { return e.LaneSetAside(abs, until) })
}

// LaneResume brings a set-aside large file back into the queue.
func (a *App) LaneResume(abs string) {
	a.firstEngine(func(e *agent.Engine) bool { return e.LaneResume(abs) })
}
