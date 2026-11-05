// Package status turns raw health signals into the components the status
// page shows: the website, Postgres, the worker and Temporal.
package status

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Yerassyl20036/filing-radar/internal/store"
)

type State string

const (
	Up    State = "up"
	Stale State = "stale" // no fresh report: may be restarting, may be down
	Down  State = "down"
)

// A worker reports every minute, so two missed reports mean it's in trouble.
const (
	freshFor = 150 * time.Second
	staleFor = 10 * time.Minute
)

type Component struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	State  State  `json:"state"`
	Note   string `json:"note"`
}

// WorkerDetail is what the worker stores in its heartbeat.
type WorkerDetail struct {
	TemporalServer string `json:"temporal_server"`
	TemporalOK     bool   `json:"temporal_ok"`
	TemporalMS     int64  `json:"temporal_ms,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Build answers "is each part working?". The web server can only see its own
// database directly; Temporal sits behind the worker, so its state comes from
// the round trip the worker last reported.
func Build(now time.Time, webServer string, dbErr error, dbLatency time.Duration, beats []store.Heartbeat) []Component {
	comps := []Component{
		{Name: "Website", Server: webServer, State: Up, Note: "answering requests"},
		database(webServer, dbErr, dbLatency),
	}

	var worker *store.Heartbeat
	for i := range beats {
		if beats[i].Service == "worker" {
			worker = &beats[i]
		}
	}
	if worker == nil {
		return append(comps,
			Component{Name: "Worker", Server: "ashburn-1", State: Down, Note: "has never reported"},
			Component{Name: "Temporal", Server: "ashburn-2", State: Stale, Note: "unknown until the worker reports"},
		)
	}

	var d WorkerDetail
	if err := json.Unmarshal(worker.Detail, &d); err != nil {
		d.Error = "unreadable heartbeat detail"
	}
	if d.TemporalServer == "" {
		d.TemporalServer = "unknown"
	}

	age := now.Sub(worker.LastSeen)
	w := Component{Name: "Worker", Server: worker.Host, Note: "last report " + ago(age)}
	t := Component{Name: "Temporal", Server: d.TemporalServer}
	switch {
	case age > staleFor:
		w.State = Down
		t.State, t.Note = Stale, "unknown while the worker is down"
		return append(comps, w, t)
	case age > freshFor:
		w.State = Stale
	default:
		w.State = Up
	}

	if d.TemporalOK {
		t.State, t.Note = Up, fmt.Sprintf("workflow round trip %d ms", d.TemporalMS)
	} else {
		t.State, t.Note = Down, "round trip failed: "+d.Error
	}
	if w.State == Stale {
		t.State = Stale
	}
	return append(comps, w, t)
}

func database(server string, err error, latency time.Duration) Component {
	c := Component{Name: "Postgres", Server: server}
	if err != nil {
		c.State, c.Note = Down, "not reachable"
		return c
	}
	c.State, c.Note = Up, fmt.Sprintf("query %d ms", latency.Milliseconds())
	return c
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1f h ago", d.Hours())
	}
}
