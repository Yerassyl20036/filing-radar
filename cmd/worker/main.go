// Command worker runs on the Ashburn-1 server. It hosts the Temporal worker
// that will run the radar's per-filing workflows, and every minute it reports
// its health to Postgres, including a full Temporal round trip.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"github.com/Yerassyl20036/filing-radar/internal/ping"
	"github.com/Yerassyl20036/filing-radar/internal/status"
	"github.com/Yerassyl20036/filing-radar/internal/store"
)

const reportEvery = time.Minute

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

	report()
	tick := time.NewTicker(reportEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("worker stopping")
			return
		case <-tick.C:
			report()
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
