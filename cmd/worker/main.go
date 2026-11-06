// Command worker runs on the Ashburn-1 server. Every minute it polls EDGAR
// for new filings and stores them, and it reports its own health to Postgres,
// including a full Temporal round trip. It also hosts the Temporal worker that
// will run the radar's per-filing workflows from stage 4.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
	"github.com/Yerassyl20036/filing-radar/internal/ingest"
	"github.com/Yerassyl20036/filing-radar/internal/ping"
	"github.com/Yerassyl20036/filing-radar/internal/status"
	"github.com/Yerassyl20036/filing-radar/internal/store"
)

const (
	reportEvery = time.Minute
	pollEvery   = time.Minute
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		fatal("open database", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		fatal("migrate", err)
	}

	c, err := dialTemporal(ctx, env("TEMPORAL_ADDRESS", "localhost:7233"))
	if err != nil {
		fatal("connect to temporal", err)
	}
	defer c.Close()

	w := worker.New(c, ping.TaskQueue, worker.Options{})
	w.RegisterWorkflow(ping.Workflow)
	w.RegisterActivity(ping.Activity)
	if err := w.Start(); err != nil {
		fatal("start temporal worker", err)
	}
	defer w.Stop()

	host := env("SERVER_NAME", "local")
	temporalServer := env("TEMPORAL_SERVER_NAME", "local")
	slog.Info("worker running", "server", host, "task_queue", ping.TaskQueue)

	// The ingestor runs beside the health reports. Polling is short and safe
	// to repeat, and its state lives in the filings table, so a plain loop is
	// enough; Temporal is for work that has to wait, like stage 4's scoring.
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		runIngestor(ctx, st, host, env("EDGAR_BASE_URL", edgar.DefaultBaseURL), os.Getenv("EDGAR_USER_AGENT"))
	})

	report := func() {
		d := status.WorkerDetail{TemporalServer: temporalServer}
		if took, err := ping.RoundTrip(ctx, c); err != nil {
			d.Error = err.Error()
			slog.Warn("temporal round trip failed", "err", err)
		} else {
			d.TemporalOK, d.TemporalMS = true, took.Milliseconds()
		}
		if err := st.Beat(ctx, "worker", host, d); err != nil {
			slog.Error("write heartbeat", "err", err)
		}
	}

	every(ctx, reportEvery, report)
	slog.Info("worker stopping")
}

// runIngestor polls EDGAR every minute until ctx ends and writes an
// "ingestor" heartbeat after each poll, so the status page shows SEC errors
// and possible gaps.
func runIngestor(ctx context.Context, st *store.Store, host, baseURL, userAgent string) {
	beat := func(d status.IngestorDetail) {
		if err := st.Beat(ctx, "ingestor", host, d); err != nil && ctx.Err() == nil {
			slog.Error("write ingestor heartbeat", "err", err)
		}
	}

	src, err := edgar.NewClient(baseURL, userAgent, time.Second)
	if err != nil {
		// SEC refuses requests without a contact address, so don't send any.
		// Keep reporting why, so the status page says what to fix.
		slog.Error("ingestor disabled", "err", err)
		every(ctx, pollEvery, func() { beat(status.IngestorDetail{Error: err.Error()}) })
		return
	}

	var lastGap *time.Time
	every(ctx, pollEvery, func() {
		pctx, cancel := context.WithTimeout(ctx, pollEvery-10*time.Second)
		defer cancel()

		res, err := ingest.Poll(pctx, src, st, ingest.MaxPages)
		if res.Gap {
			now := time.Now().UTC()
			lastGap = &now
			slog.Warn("possible gap: read the page cap without reaching stored filings", "pages", res.Pages)
		}
		d := status.IngestorDetail{
			Pages: res.Pages, Filings: res.Filings, New: res.New, Skipped: res.Skipped,
			TookMS: res.Took.Milliseconds(), LastGapAt: lastGap,
		}
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			d.Error = err.Error()
			slog.Warn("poll failed", "pages", res.Pages, "err", err)
		} else {
			slog.Info("poll", "pages", res.Pages, "filings", res.Filings, "new", res.New, "skipped", res.Skipped, "took", res.Took.Round(time.Millisecond))
		}
		beat(d)
	})
}

// every runs fn now and then every interval until ctx ends.
func every(ctx context.Context, interval time.Duration, fn func()) {
	fn()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			fn()
		}
	}
}

// dialTemporal keeps trying until the Temporal server answers. Like Postgres,
// it is reached through an SSH tunnel that can take a moment to come up.
func dialTemporal(ctx context.Context, addr string) (client.Client, error) {
	for attempt := 1; ; attempt++ {
		c, err := client.DialContext(ctx, client.Options{
			HostPort: addr,
			Logger:   tlog.NewStructuredLogger(slog.Default()),
		})
		if err == nil {
			return c, nil
		}
		wait := time.Duration(min(attempt, 10)) * time.Second
		slog.Warn("temporal not reachable yet", "addr", addr, "attempt", attempt, "retry_in", wait, "err", err)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fatal("missing configuration", errors.New(key+" is not set"))
	}
	return v
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
