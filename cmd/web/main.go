// Command web serves the radar's website and API from the Frankfurt server:
// the filing feed, a live status page and a "how it works" page.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Yerassyl20036/filing-radar/internal/status"
	"github.com/Yerassyl20036/filing-radar/internal/store"
)

// The pages and stylesheet are built into the binary, so a deploy is one image.
//
//go:embed static
var static embed.FS

func main() {
	healthcheck := flag.Bool("healthcheck", false, "check /healthz on ADDR and exit; used by the Docker health check")
	flag.Parse()

	addr := env("ADDR", ":8080")
	if *healthcheck {
		os.Exit(checkHealth(addr))
	}

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

	s := &server{store: st, name: env("SERVER_NAME", "local")}
	mux := http.NewServeMux()
	routePages(mux)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/costs", s.costs)
	mux.HandleFunc("GET /api/scorecard", s.scorecard)
	mux.HandleFunc("GET /api/filings", s.filings)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	slog.Info("web listening", "addr", addr, "server", s.name)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("serve", err)
	}
}

type server struct {
	store *store.Store
	name  string // which machine this is, shown on the status page
}

// routePages adds the pages, which need no database.
func routePages(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", page("static/feed.html"))
	mux.HandleFunc("GET /status", page("static/status.html"))
	mux.HandleFunc("GET /how-it-works", page("static/how.html"))
	mux.HandleFunc("GET /about", page("static/about.html"))
	// Some clients ask for /favicon.ico regardless of the <link> tag.
	mux.Handle("GET /favicon.ico", http.RedirectHandler("/static/logo.svg", http.StatusMovedPermanently))
	mux.Handle("GET /static/", http.FileServerFS(static))
}

// page serves one embedded HTML file. A missing file is a build mistake, so it
// fails at startup rather than on the first request.
func page(name string) http.HandlerFunc {
	body, err := static.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	start := time.Now()
	dbErr := s.store.Ping(ctx)
	latency := time.Since(start)

	var (
		beats      []store.Heartbeat
		backlog    store.Backlog
		backlogErr error
		spend      status.Spend
	)
	if dbErr == nil {
		var err error
		if beats, err = s.store.Heartbeats(ctx); err != nil {
			slog.Error("read heartbeats", "err", err)
		}
		if backlog, backlogErr = s.store.TriageBacklog(ctx); backlogErr != nil {
			slog.Error("read triage backlog", "err", backlogErr)
		}
		if spend.USD, spend.Calls, spend.Err = s.store.ModelSpendToday(ctx); spend.Err != nil {
			slog.Error("read model spend", "err", spend.Err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"checked_at": time.Now().UTC(),
		"components": status.Build(time.Now(), s.name, dbErr, latency, beats, backlog, backlogErr, spend),
	})
}

// checkHealth calls our own /healthz. The runtime image has no shell or curl,
// so the binary checks itself.
func checkHealth(addr string) int {
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + host + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
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
