package agent

import (
	"testing"
	"time"
)

// newNowTestEngine is a bare Engine; the snapshot state is created on first use.
func newNowTestEngine() *Engine { return &Engine{} }

func TestNowJobsTracksAnUploadToTheEnd(t *testing.T) {
	e := newNowTestEngine()
	abs := `C:\x\a.txt`
	done := e.jobStart(abs, "upload", 100)
	e.jobProgress(abs, 40)

	jobs := e.NowJobs()
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d: %+v", len(jobs), jobs)
	}
	if j := jobs[0]; j.Kind != "upload" || j.Size != 100 || j.Sent != 40 || j.State != "running" || j.Large {
		t.Fatalf("unexpected job: %+v", j)
	}

	done(nil)
	done(nil) // ending twice is harmless
	if jobs := e.NowJobs(); len(jobs) != 0 {
		t.Fatalf("want no jobs after done, got %+v", jobs)
	}
}

func TestScanNowCarriesTheCount(t *testing.T) {
	e := newNowTestEngine()
	e.scanBegin()
	e.scanTickCount("server", 1240, "TerraVia/Clients")
	want := ScanNow{Active: true, Phase: "server", Count: 1240, Folder: "TerraVia/Clients"}
	if got := e.ScanNow(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	e.scanEnd()
	if got := e.ScanNow(); got.Active {
		t.Fatalf("scan should be inactive after scanEnd: %+v", got)
	}
}

func TestLastFullCheckIgnoresFailedPasses(t *testing.T) {
	e := newNowTestEngine()
	t1 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	e.noteFullCheck(t1, true)
	e.noteFullCheck(t2, false)
	at, incomplete := e.LastFullCheck()
	if !at.Equal(t1) || !incomplete {
		t.Fatalf("got (%v, %v), want (%v, true)", at, incomplete, t1)
	}
}
