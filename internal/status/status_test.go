package status

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Yerassyl20036/filing-radar/internal/store"
)

func worker(t *testing.T, age time.Duration, d WorkerDetail, now time.Time) store.Heartbeat {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return store.Heartbeat{Service: "worker", Host: "ashburn-1", Detail: raw, LastSeen: now.Add(-age)}
}

func states(comps []Component) map[string]State {
	m := map[string]State{}
	for _, c := range comps {
		m[c.Name] = c.State
	}
	return m
}

func named(comps []Component, name string) Component {
	for _, c := range comps {
		if c.Name == name {
			return c
		}
	}
	return Component{}
}

func TestBuild(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	ok := WorkerDetail{TemporalServer: "ashburn-2", TemporalOK: true, TemporalMS: 42}
	failed := WorkerDetail{TemporalServer: "ashburn-2", Error: "deadline exceeded"}

	tests := []struct {
		name  string
		dbErr error
		beats []store.Heartbeat
		want  map[string]State
	}{
		{"all healthy", nil, []store.Heartbeat{worker(t, 30*time.Second, ok, now)},
			map[string]State{"Website": Up, "Postgres": Up, "Worker": Up, "Temporal": Up}},
		{"worker never reported", nil, nil,
			map[string]State{"Website": Up, "Postgres": Up, "Worker": Down, "Temporal": Stale}},
		{"temporal round trip failed", nil, []store.Heartbeat{worker(t, 30*time.Second, failed, now)},
			map[string]State{"Website": Up, "Postgres": Up, "Worker": Up, "Temporal": Down}},
		{"worker missed two reports", nil, []store.Heartbeat{worker(t, 4*time.Minute, ok, now)},
			map[string]State{"Website": Up, "Postgres": Up, "Worker": Stale, "Temporal": Stale}},
		{"worker silent for an hour", nil, []store.Heartbeat{worker(t, time.Hour, ok, now)},
			map[string]State{"Website": Up, "Postgres": Up, "Worker": Down, "Temporal": Stale}},
		{"database down", errors.New("refused"), nil,
			map[string]State{"Website": Up, "Postgres": Down, "Worker": Down, "Temporal": Stale}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := states(Build(now, "frankfurt-1", tt.dbErr, 3*time.Millisecond, tt.beats, store.Backlog{}, nil))
			for name, want := range tt.want {
				if got[name] != want {
					t.Errorf("%s = %q, want %q", name, got[name], want)
				}
			}
		})
	}
}

func ingestorBeat(t *testing.T, age time.Duration, d IngestorDetail, now time.Time) store.Heartbeat {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return store.Heartbeat{Service: "ingestor", Host: "ashburn-1", Detail: raw, LastSeen: now.Add(-age)}
}

func TestBuildIngestor(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	gapAt := now.Add(-time.Hour)
	tests := []struct {
		name     string
		beats    []store.Heartbeat
		want     State
		wantNote string
	}{
		{"never polled", nil, Down, "has never polled EDGAR"},
		{"healthy", []store.Heartbeat{ingestorBeat(t, 20*time.Second, IngestorDetail{Pages: 1, New: 3}, now)},
			Up, "last poll 20 s ago: 3 new filings, 1 page"},
		{"SEC refusing", []store.Heartbeat{ingestorBeat(t, 20*time.Second, IngestorDetail{Error: "SEC answered 429: too many requests"}, now)},
			Down, "last poll failed: SEC answered 429: too many requests"},
		{"missed polls", []store.Heartbeat{ingestorBeat(t, 4*time.Minute, IngestorDetail{Pages: 1, New: 1}, now)},
			Stale, "last poll 4 min ago: 1 new filing, 1 page"},
		{"gap an hour ago", []store.Heartbeat{ingestorBeat(t, 10*time.Second, IngestorDetail{Pages: 1, LastGapAt: &gapAt}, now)},
			Up, "last poll 10 s ago: 0 new filings, 1 page; possible gap 1.0 h ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := named(Build(now, "frankfurt-1", nil, time.Millisecond, tt.beats, store.Backlog{}, nil), "Ingestor")
			if got.Name != "Ingestor" || got.State != tt.want || got.Note != tt.wantNote {
				t.Errorf("got %s %s %q, want %s %q", got.Name, got.State, got.Note, tt.want, tt.wantNote)
			}
		})
	}
}

func TestBuildTriage(t *testing.T) {
	now := time.Date(2026, 11, 6, 16, 0, 0, 0, time.UTC)
	waiting := func(n int, age time.Duration) store.Backlog {
		return store.Backlog{Pending: n, Oldest: now.Add(-age)}
	}
	tests := []struct {
		name       string
		dbErr      error
		backlog    store.Backlog
		backlogErr error
		want       State
		wantNote   string
	}{
		{"database down", errors.New("refused"), store.Backlog{}, nil, Down, "backlog unavailable"},
		{"backlog query failed", nil, store.Backlog{}, errors.New("timeout"), Down, "backlog unavailable"},
		{"nothing waiting", nil, store.Backlog{}, nil, Up, "every filing labelled"},
		{"a few just in", nil, waiting(3, 2*time.Minute), nil, Up, "3 filings waiting, oldest 2 min ago"},
		{"one just in", nil, waiting(1, 40*time.Second), nil, Up, "1 filing waiting, oldest 40 s ago"},
		{"falling behind", nil, waiting(12, 10*time.Minute), nil, Stale, "12 filings waiting, oldest 10 min ago"},
		{"still behind at 30 min", nil, waiting(12, 30*time.Minute), nil, Stale, "12 filings waiting, oldest 30 min ago"},
		{"stopped", nil, waiting(40, 45*time.Minute), nil, Down, "40 filings waiting, oldest 45 min ago"},
		{"stopped for hours", nil, waiting(400, 3*time.Hour), nil, Down, "400 filings waiting, oldest 3.0 h ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comps := Build(now, "frankfurt-1", tt.dbErr, time.Millisecond, nil, tt.backlog, tt.backlogErr)
			got := comps[len(comps)-1] // the status page lists triage last, after the ingestor
			if got.Name != "Triage" || got.Server != "ashburn-1" || got.State != tt.want || got.Note != tt.wantNote {
				t.Errorf("got %s on %s %s %q, want Triage on ashburn-1 %s %q", got.Name, got.Server, got.State, got.Note, tt.want, tt.wantNote)
			}
		})
	}
}
