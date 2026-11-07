// Command worker runs on the Ashburn-1 server. Every minute it polls EDGAR
// for new filings, stores them, and hands each unlabelled one to triage: a
// label on the spot for routine forms, a Temporal workflow for the rest. It
// also runs those workflows, and reports its own health to Postgres,
// including a full Temporal round trip.
package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // notes and market closes use New York time; the distroless image has no zone files

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
	"github.com/Yerassyl20036/filing-radar/internal/ingest"
	"github.com/Yerassyl20036/filing-radar/internal/model"
	"github.com/Yerassyl20036/filing-radar/internal/ping"
	"github.com/Yerassyl20036/filing-radar/internal/pipeline"
	"github.com/Yerassyl20036/filing-radar/internal/prices"
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

	// One SEC client for the poller and the triage workflows, so they share
	// one budget of a request per second. Without a contact address it's nil
	// and nothing is sent to SEC.
	sec, secErr := edgar.NewClient(edgar.Config{
		BaseURL:   env("EDGAR_BASE_URL", edgar.DefaultBaseURL),
		DataURL:   env("EDGAR_DATA_URL", edgar.DefaultDataURL),
		UserAgent: os.Getenv("EDGAR_USER_AGENT"),
		MinGap:    time.Second,
	})
	acts := &pipeline.Activities{
		Store:    st,
		BaseURL:  env("EDGAR_BASE_URL", edgar.DefaultBaseURL),
		DailyCap: envInt("TRIAGE_MODEL_DAILY_CAP", 200),
		Budget:   envFloat("MODEL_DAILY_BUDGET_USD", 0.20),
		Hold:     &pipeline.Hold{},
		Prices:   prices.NewYahoo(env("PRICES_BASE_URL", prices.DefaultBaseURL)),
	}
	if secErr == nil {
		// Only when set: a nil *Client inside an interface wouldn't be nil.
		acts.SEC, acts.Docs, acts.Archive = sec, sec, sec
		// SEC's ticker list is about 1 MB and changes slowly: once a day.
		acts.Tickers = &pipeline.TickerCache{Fetch: sec.Tickers, TTL: 24 * time.Hour}
	}
	noteCap := envInt("ANALYST_DAILY_CAP", 30)
	// Without a key the rules label everything on their own, which is how
	// stage 2a ran, and no notes are written. Keys never leave the worker's
	// environment.
	if m, provider := chooseModel(); m != nil {
		acts.Model, acts.Writer, acts.Provider = m, m, provider
		slog.Info("model on", "provider", provider, "model", os.Getenv("TRIAGE_MODEL"), "daily_cap", acts.DailyCap,
			"budget_usd", acts.Budget, "analyst_daily_cap", noteCap)
	} else {
		noteCap = 0
		slog.Info("model off: no model API key is set")
	}

	w := worker.New(c, ping.TaskQueue, worker.Options{
		// Activities wait their turn for SEC; a few at a time keeps any one
		// of them from timing out in the queue.
		MaxConcurrentActivityExecutionSize: 8,
	})
	w.RegisterWorkflow(ping.Workflow)
	w.RegisterActivity(ping.Activity)
	w.RegisterWorkflow(pipeline.FilingWorkflow)
	w.RegisterWorkflow(pipeline.NoteWorkflow)
	w.RegisterWorkflow(pipeline.OutcomeWorkflow)
	w.RegisterActivity(acts)
	if err := w.Start(); err != nil {
		fatal("start temporal worker", err)
	}
	defer w.Stop()

	host := env("SERVER_NAME", "local")
	temporalServer := env("TEMPORAL_SERVER_NAME", "local")
	slog.Info("worker running", "server", host, "task_queue", ping.TaskQueue)

	// The ingestor runs beside the health reports. Polling is short and safe
	// to repeat, and its state lives in the filings table, so a plain loop is
	// enough. Temporal is for the multi-step work per filing.
	dispatcher := &pipeline.Dispatcher{Store: st, Temporal: c, Batch: 200, NoteCap: noteCap, Budget: acts.Budget, Hold: acts.Hold,
		Outcomes: secErr == nil}
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { runIngestor(ctx, st, host, sec, secErr, dispatcher) })

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
// and possible gaps. After each poll it dispatches unlabelled filings, new
// or left over, so triage catches up after any outage.
func runIngestor(ctx context.Context, st *store.Store, host string, src *edgar.Client, srcErr error, dispatcher *pipeline.Dispatcher) {
	beat := func(d status.IngestorDetail) {
		if err := st.Beat(ctx, "ingestor", host, d); err != nil && ctx.Err() == nil {
			slog.Error("write ingestor heartbeat", "err", err)
		}
	}

	if srcErr != nil {
		// SEC refuses requests without a contact address, so don't send any.
		// Keep reporting why, so the status page says what to fix.
		slog.Error("ingestor disabled", "err", srcErr)
		every(ctx, pollEvery, func() { beat(status.IngestorDetail{Error: srcErr.Error()}) })
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

		// Even after a failed poll: earlier filings may still be waiting.
		dctx, dcancel := context.WithTimeout(ctx, 30*time.Second)
		defer dcancel()
		dr, err := dispatcher.Dispatch(dctx)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Warn("dispatch", "labelled", dr.Labelled, "started", dr.Started, "running", dr.Running,
				"notes_started", dr.NotesStarted, "notes_running", dr.NotesRunning,
				"outcomes_started", dr.OutcomesStarted, "outcomes_running", dr.OutcomesRunning, "failed", dr.Failed, "err", err)
		case dr != (pipeline.DispatchResult{}):
			slog.Info("dispatch", "labelled", dr.Labelled, "started", dr.Started, "running", dr.Running,
				"notes_started", dr.NotesStarted, "notes_running", dr.NotesRunning,
				"outcomes_started", dr.OutcomesStarted, "outcomes_running", dr.OutcomesRunning)
		}
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

// chooseModel picks the model that scores 8-Ks and writes notes.
// TRIAGE_PROVIDER names it (deepseek, gemini, groq or anthropic); left empty,
// the first provider with a key set wins, in that order. TRIAGE_MODEL
// overrides the provider's default model, and MODEL_PRICE_IN_PER_M with
// MODEL_PRICE_OUT_PER_M its list prices, for when a provider changes them.
func chooseModel() (pipeline.Model, string) {
	keys := map[string]string{
		"deepseek":  os.Getenv("DEEPSEEK_API_KEY"),
		"gemini":    os.Getenv("GEMINI_API_KEY"),
		"groq":      os.Getenv("GROQ_API_KEY"),
		"anthropic": os.Getenv("ANTHROPIC_API_KEY"),
	}
	provider := os.Getenv("TRIAGE_PROVIDER")
	if provider == "" {
		for _, p := range []string{"deepseek", "gemini", "groq", "anthropic"} {
			if keys[p] != "" {
				provider = p
				break
			}
		}
		if provider == "" {
			return nil, ""
		}
	}
	key, known := keys[provider]
	if !known {
		fatal("bad configuration", errors.New("TRIAGE_PROVIDER must be deepseek, gemini, groq or anthropic"))
	}
	if key == "" {
		fatal("bad configuration", errors.New("TRIAGE_PROVIDER is "+provider+" but its API key is not set"))
	}
	name := os.Getenv("TRIAGE_MODEL")
	var m interface {
		pipeline.Model
		SetPrices(inPerM, outPerM float64)
	}
	if provider == "anthropic" {
		m = model.NewClaude(key, name)
	} else {
		m = model.NewOpenAICompatible(model.Presets[provider], key, name, os.Getenv("TRIAGE_BASE_URL"))
	}
	in, out := os.Getenv("MODEL_PRICE_IN_PER_M"), os.Getenv("MODEL_PRICE_OUT_PER_M")
	switch {
	case in != "" && out != "":
		m.SetPrices(envFloat("MODEL_PRICE_IN_PER_M", 0), envFloat("MODEL_PRICE_OUT_PER_M", 0))
	case in != "" || out != "":
		fatal("bad configuration", errors.New("set both MODEL_PRICE_IN_PER_M and MODEL_PRICE_OUT_PER_M, or neither"))
	}
	return m, provider
}

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		fatal("bad configuration", errors.New(key+" must be a number, 0 or more"))
	}
	return f
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		fatal("bad configuration", errors.New(key+" must be a whole number"))
	}
	return n
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
