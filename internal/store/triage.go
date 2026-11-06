package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the agreed interface for stage 2a's storage.

// Triage is the stored label for one filing, plus the 8-K item numbers from
// filing_details so the API can show both together.
type Triage struct {
	AccessionNo  string    `json:"-"`
	Level        string    `json:"level"` // high, medium, low or routine
	Reasons      []string  `json:"reasons"`
	Items        []string  `json:"items"` // read from filing_details; empty if none
	RulesVersion int       `json:"rules_version"`
	ModelScore   *int      `json:"model_score,omitempty"` // stage 2b
	ModelReason  string    `json:"model_reason,omitempty"`
	ModelQuote   string    `json:"model_quote,omitempty"` // a passage from the filing, verified to be there
	Model        string    `json:"model,omitempty"`
	DecidedAt    time.Time `json:"decided_at"`
}

// Undecided is a filing that needs labelling: it has no triage row yet, or
// its label came from older rules.
type Undecided struct {
	AccessionNo  string
	FormType     string
	CIK          string
	Company      string
	FirstSeenAt  time.Time
	RulesVersion int // 0 when there's no label yet
}

// Backlog is how far behind triage is.
type Backlog struct {
	Pending int       // filings with no triage row
	Oldest  time.Time // first_seen_at of the oldest of them; zero if none
}

// SaveDetails stores what SEC's per-company JSON said about a filing.
// Saving again overwrites.
func (s *Store) SaveDetails(ctx context.Context, accessionNo string, items []string, primaryDoc string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO filing_details (accession_no, items, primary_doc, fetched_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (accession_no) DO UPDATE
		SET items = EXCLUDED.items, primary_doc = EXCLUDED.primary_doc, fetched_at = EXCLUDED.fetched_at`,
		accessionNo, orEmpty(items), primaryDoc)
	if err != nil {
		return fmt.Errorf("save details of %s: %w", accessionNo, err)
	}
	return nil
}

// Details returns stored details, so re-labelling never asks SEC again.
func (s *Store) Details(ctx context.Context, accessionNo string) (items []string, primaryDoc string, found bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT items, primary_doc FROM filing_details WHERE accession_no = $1`, accessionNo).
		Scan(&items, &primaryDoc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("read details of %s: %w", accessionNo, err)
	}
	return orEmpty(items), primaryDoc, true, nil
}

// SaveTriage stores a label. Saving again overwrites (an upsert), so a
// retried activity can't create a second row.
func (s *Store) SaveTriage(ctx context.Context, t Triage) error {
	// Items and DecidedAt are not written: items belong to filing_details,
	// and the database clock says when a label was made.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO triage (accession_no, level, reasons, rules_version, model_score, model_reason, model_quote, model, decided_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (accession_no) DO UPDATE
		SET level = EXCLUDED.level, reasons = EXCLUDED.reasons, rules_version = EXCLUDED.rules_version,
		    model_score = EXCLUDED.model_score, model_reason = EXCLUDED.model_reason,
		    model_quote = EXCLUDED.model_quote, model = EXCLUDED.model, decided_at = EXCLUDED.decided_at`,
		t.AccessionNo, t.Level, orEmpty(t.Reasons), t.RulesVersion, t.ModelScore, t.ModelReason, t.ModelQuote, t.Model)
	if err != nil {
		return fmt.Errorf("save triage of %s: %w", t.AccessionNo, err)
	}
	return nil
}

// Undecided lists filings with no triage row, or one made by rules older than
// rulesVersion, oldest first.
func (s *Store) Undecided(ctx context.Context, rulesVersion, limit int) ([]Undecided, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT f.accession_no, f.form_type, f.cik, f.company, f.first_seen_at, coalesce(t.rules_version, 0)
		FROM filings f
		LEFT JOIN triage t ON t.accession_no = f.accession_no
		WHERE t.accession_no IS NULL OR t.rules_version < $1
		ORDER BY f.first_seen_at, f.accession_no
		LIMIT $2`,
		rulesVersion, limit)
	if err != nil {
		return nil, fmt.Errorf("list undecided filings: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Undecided, error) {
		var u Undecided
		err := row.Scan(&u.AccessionNo, &u.FormType, &u.CIK, &u.Company, &u.FirstSeenAt, &u.RulesVersion)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("list undecided filings: %w", err)
	}
	return out, nil
}

// TriageBacklog counts filings with no triage row.
func (s *Store) TriageBacklog(ctx context.Context) (Backlog, error) {
	var (
		b      Backlog
		oldest *time.Time // min() over no rows is NULL
	)
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(f.first_seen_at)
		FROM filings f
		WHERE NOT EXISTS (SELECT 1 FROM triage t WHERE t.accession_no = f.accession_no)`).
		Scan(&b.Pending, &oldest)
	if err != nil {
		return Backlog{}, fmt.Errorf("count triage backlog: %w", err)
	}
	if oldest != nil {
		b.Oldest = *oldest
	}
	return b, nil
}

// orEmpty turns nil into an empty slice. pgx sends a nil slice as NULL, which
// the NOT NULL array columns refuse, and the API should show [] rather than null.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ModelCallsToday counts labels the model contributed to since midnight UTC,
// so the daily cap holds across restarts.
func (s *Store) ModelCallsToday(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM triage
		WHERE model <> '' AND decided_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count model calls: %w", err)
	}
	return n, nil
}
