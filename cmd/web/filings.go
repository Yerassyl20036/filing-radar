package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Yerassyl20036/filing-radar/internal/store"
)

const (
	defaultLimit = 50
	maxLimit     = 100
)

// filings serves GET /api/filings?form=8-K&limit=50&before=<cursor>, newest
// first. "next" is the cursor for the following page, or "" at the end.
func (s *server) filings(w http.ResponseWriter, r *http.Request) {
	q, err := parseFilingQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	rows, err := s.store.Filings(ctx, q)
	if err != nil {
		slog.Error("read filings", "err", err)
		http.Error(w, "filings are unavailable right now", http.StatusServiceUnavailable)
		return
	}

	next := ""
	if len(rows) == q.Limit {
		last := rows[len(rows)-1]
		next = encodeCursor(store.Cursor{AcceptedAt: last.AcceptedAt, AccessionNo: last.AccessionNo})
	}
	if rows == nil {
		rows = []store.FilingRow{} // [] in JSON, not null
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"filings": rows, "next": next})
}

func parseFilingQuery(r *http.Request) (store.FilingQuery, error) {
	v := r.URL.Query()
	q := store.FilingQuery{FormType: strings.TrimSpace(v.Get("form")), Limit: defaultLimit}
	if len(q.FormType) > 32 {
		return q, errors.New("form is too long")
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxLimit {
			return q, errors.New("limit must be a number from 1 to 100")
		}
		q.Limit = n
	}
	if s := v.Get("before"); s != "" {
		c, err := decodeCursor(s)
		if err != nil {
			return q, errors.New("before is not a cursor this API returned")
		}
		q.Before = &c
	}
	return q, nil
}

// The cursor is opaque to clients: base64 of "<RFC 3339 time>|<accession>".
// Opaque means the format can change without breaking anyone.
func encodeCursor(c store.Cursor) string {
	raw := c.AcceptedAt.UTC().Format(time.RFC3339Nano) + "|" + c.AccessionNo
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (store.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.Cursor{}, err
	}
	at, acc, ok := strings.Cut(string(raw), "|")
	if !ok || acc == "" {
		return store.Cursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return store.Cursor{}, err
	}
	return store.Cursor{AcceptedAt: t, AccessionNo: acc}, nil
}
