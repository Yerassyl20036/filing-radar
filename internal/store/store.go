// Package store is the radar's Postgres access.
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

type Store struct {
	pool *pgxpool.Pool
}

// Heartbeat is the latest report from one long-running service.
type Heartbeat struct {
	Service  string          `json:"service"`
	Host     string          `json:"host"`
	Detail   json.RawMessage `json:"detail"`
	LastSeen time.Time       `json:"last_seen"`
}

// Open connects to Postgres and keeps retrying until ctx ends. On the worker
// the database sits behind an SSH tunnel that may come up a few seconds after
// the worker does, so failing on the first attempt would only cause restarts.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 4 // 1 GB servers: keep Postgres connections few

	for attempt := 1; ; attempt++ {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return &Store{pool: pool}, nil
			}
			pool.Close()
		}
		wait := time.Duration(min(attempt, 10)) * time.Second
		slog.Warn("postgres not reachable yet", "attempt", attempt, "retry_in", wait, "err", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to postgres: %w", err)
		case <-time.After(wait):
		}
	}
}

func (s *Store) Close() { s.pool.Close() }

// Migrate creates the tables if they don't exist yet. Every statement in
// schema.sql must be safe to run again.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Beat records that service on host is alive, with any detail it wants to show.
func (s *Store) Beat(ctx context.Context, service, host string, detail any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode heartbeat detail: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO heartbeats (service, host, detail, last_seen)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (service) DO UPDATE
		SET host = EXCLUDED.host, detail = EXCLUDED.detail, last_seen = EXCLUDED.last_seen`,
		service, host, raw)
	if err != nil {
		return fmt.Errorf("write heartbeat: %w", err)
	}
	return nil
}

func (s *Store) Heartbeats(ctx context.Context) ([]Heartbeat, error) {
	rows, err := s.pool.Query(ctx, `SELECT service, host, detail, last_seen FROM heartbeats ORDER BY service`)
	if err != nil {
		return nil, fmt.Errorf("read heartbeats: %w", err)
	}
	defer rows.Close()

	var beats []Heartbeat
	for rows.Next() {
		var b Heartbeat
		if err := rows.Scan(&b.Service, &b.Host, &b.Detail, &b.LastSeen); err != nil {
			return nil, fmt.Errorf("read heartbeats: %w", err)
		}
		beats = append(beats, b)
	}
	return beats, rows.Err()
}
