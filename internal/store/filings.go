package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
)

// FilingRow is a stored filing, as the API returns it.
type FilingRow struct {
	AccessionNo string          `json:"accession_no"`
	FormType    string          `json:"form_type"`
	CIK         string          `json:"cik"`
	Company     string          `json:"company"`
	AcceptedAt  time.Time       `json:"accepted_at"`
	URL         string          `json:"url"`
	Parties     json.RawMessage `json:"parties"`
	FirstSeenAt time.Time       `json:"first_seen_at"`
}

// Cursor marks the last row of a page; the next page starts after it.
// accession_no breaks ties: many filings share the same second.
type Cursor struct {
	AcceptedAt  time.Time
	AccessionNo string
}

type FilingQuery struct {
	FormType string  // "" for every form
	Limit    int     // at most this many rows
	Before   *Cursor // nil for the newest
}

// InsertFilings stores filings it hasn't seen and ignores the rest, so a
// retry or an overlapping poll can never create a duplicate. It returns how
// many were new.
func (s *Store) InsertFilings(ctx context.Context, filings []edgar.Filing) (int, error) {
	if len(filings) == 0 {
		return 0, nil
	}
	b := &pgx.Batch{}
	for _, f := range filings {
		parties, err := json.Marshal(f.Parties)
		if err != nil {
			return 0, fmt.Errorf("encode parties of %s: %w", f.AccessionNo, err)
		}
		b.Queue(`
			INSERT INTO filings (accession_no, form_type, cik, company, accepted_at, url, parties)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (accession_no) DO NOTHING`,
			f.AccessionNo, f.FormType, f.CIK, f.Company, f.AcceptedAt, f.URL, parties)
	}

	// One round trip for the whole batch, which matters through a tunnel
	// from Ashburn to Frankfurt (about 90 ms each way).
	br := s.pool.SendBatch(ctx, b)
	inserted := 0
	for range filings {
		tag, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return inserted, fmt.Errorf("insert filings: %w", err)
		}
		inserted += int(tag.RowsAffected())
	}
	if err := br.Close(); err != nil {
		return inserted, fmt.Errorf("insert filings: %w", err)
	}
	return inserted, nil
}

// KnownAccessions says which of these accession numbers are already stored.
func (s *Store) KnownAccessions(ctx context.Context, accessionNos []string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT accession_no FROM filings WHERE accession_no = ANY($1)`, accessionNos)
	if err != nil {
		return nil, fmt.Errorf("look up filings: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("look up filings: %w", err)
	}
	known := make(map[string]bool, len(found))
	for _, a := range found {
		known[a] = true
	}
	return known, nil
}

// Filings returns stored filings newest first. The WHERE clause is built
// from only the filters in use, so Postgres can pick the matching index.
func (s *Store) Filings(ctx context.Context, q FilingQuery) ([]FilingRow, error) {
	var (
		where []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if q.FormType != "" {
		where = append(where, "form_type = "+arg(q.FormType))
	}
	if q.Before != nil {
		where = append(where, fmt.Sprintf("(accepted_at, accession_no) < (%s, %s)", arg(q.Before.AcceptedAt), arg(q.Before.AccessionNo)))
	}

	sql := `SELECT accession_no, form_type, cik, company, accepted_at, url, parties, first_seen_at FROM filings`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY accepted_at DESC, accession_no DESC LIMIT " + arg(q.Limit)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read filings: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (FilingRow, error) {
		var f FilingRow
		err := row.Scan(&f.AccessionNo, &f.FormType, &f.CIK, &f.Company, &f.AcceptedAt, &f.URL, &f.Parties, &f.FirstSeenAt)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("read filings: %w", err)
	}
	return out, nil
}
