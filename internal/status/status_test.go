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
			got := states(Build(now, "frankfurt-1", tt.dbErr, 3*time.Millisecond, tt.beats))
			for name, want := range tt.want {
				if got[name] != want {
					t.Errorf("%s = %q, want %q", name, got[name], want)
				}
			}
		})
	}
}
