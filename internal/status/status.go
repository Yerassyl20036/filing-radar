// Package status turns raw health signals into the components the status
// page shows: the website, Postgres, the worker, Temporal and the ingestor.
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

// IngestorDetail is what the worker stores after each poll of EDGAR.
type IngestorDetail struct {
	Pages     int        `json:"pages"`
	Filings   int        `json:"filings"`
	New       int        `json:"new"`
	Skipped   int        `json:"skipped,omitempty"`
	TookMS    int64      `json:"took_ms"`
	LastGapAt *time.Time `json:"last_gap_at,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Build answers "is each part working?". The web server can only see its own
// database directly; Temporal sits behind the worker, so its state comes from
// the round trip the worker last reported.
func Build(now time.Time, webServer string, dbErr error, dbLatency time.Duration, beats []store.Heartbeat) []Component {
	comps := []Component{
		{Name: "Website", Server: webServer, State: Up, Note: "answering requests"},
		database(webServer, dbErr, dbLatency),
	}
	comps = append(comps, workerAndTemporal(now, find(beats, "worker"))...)
	return append(comps, ingestor(now, find(beats, "ingestor")))
}

func find(beats []store.Heartbeat, service string) *store.Heartbeat {
	for i := range beats {
		if beats[i].Service == service {
			return &beats[i]
		}
	}
	return nil
}

func workerAndTemporal(now time.Time, worker *store.Heartbeat) []Component {
	if worker == nil {
		return []Component{
			{Name: "Worker", Server: "ashburn-1", State: Down, Note: "has never reported"},
			{Name: "Temporal", Server: "ashburn-2", State: Stale, Note: "unknown until the worker reports"},
		}
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
		return []Component{w, t}
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
	return []Component{w, t}
}

// ingestor shows the EDGAR poller. It runs inside the worker but reports on
// its own, so "the worker is fine but SEC is refusing us" is visible.
func ingestor(now time.Time, beat *store.Heartbeat) Component {
	c := Component{Name: "Ingestor", Server: "ashburn-1"}
	if beat == nil {
		c.State, c.Note = Down, "has never polled EDGAR"
		return c
	}
	c.Server = beat.Host

	var d IngestorDetail
	if err := json.Unmarshal(beat.Detail, &d); err != nil {
		d.Error = "unreadable heartbeat detail"
	}
	age := now.Sub(beat.LastSeen)
	switch {
	case age > staleFor:
		c.State, c.Note = Down, "last poll "+ago(age)
		return c
	case d.Error != "":
		c.State, c.Note = Down, "last poll failed: "+d.Error
		return c
	case age > freshFor:
		c.State = Stale
	default:
		c.State = Up
	}

	c.Note = fmt.Sprintf("last poll %s: %s, %s", ago(age), count(d.New, "new filing"), count(d.Pages, "page"))
	if d.LastGapAt != nil && now.Sub(*d.LastGapAt) < 24*time.Hour {
		c.Note += "; possible gap " + ago(now.Sub(*d.LastGapAt))
	}
	return c
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
