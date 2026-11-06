package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Claim is one checked sentence of a note.
type Claim struct {
	Text   string `json:"text"`
	Quote  string `json:"quote"`  // word for word from its source
	Source string `json:"source"` // "current" or "previous"
	URL    string `json:"url"`    // the source document on sec.gov
}

// Note is the analyst's note on one filing.
type Note struct {
	AccessionNo         string    `json:"-"`
	Status              string    `json:"status"` // "published" or "rejected"
	Headline            string    `json:"headline"`
	Claims              []Claim   `json:"claims"`
	PreviousAccessionNo string    `json:"previous_accession_no,omitempty"`
	Reason              string    `json:"reason,omitempty"` // why it was rejected
	Model               string    `json:"model"`
	PromptVersion       int       `json:"prompt_version"`
	CreatedAt           time.Time `json:"created_at"`
}

// NoteTask is a filing that should get a note: a high 8-K with details and
// no note yet.
type NoteTask struct {
	AccessionNo string
	FormType    string
	CIK         string
	Company     string
	Items       []string
	PrimaryDoc  string
	AcceptedAt  time.Time
}

// SaveNote stores a note. Saving again overwrites (an upsert), and the note
// counts as new: a re-written note is a different note, so created_at moves.
func (s *Store) SaveNote(ctx context.Context, n Note) error {
	claims := n.Claims
	if claims == nil {
		claims = []Claim{} // nil would encode as JSON null
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return fmt.Errorf("encode claims of %s: %w", n.AccessionNo, err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO notes (accession_no, status, headline, claims, previous_accession_no, reason, model, prompt_version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (accession_no) DO UPDATE
		SET status = EXCLUDED.status, headline = EXCLUDED.headline, claims = EXCLUDED.claims,
			previous_accession_no = EXCLUDED.previous_accession_no, reason = EXCLUDED.reason,
			model = EXCLUDED.model, prompt_version = EXCLUDED.prompt_version, created_at = EXCLUDED.created_at`,
		n.AccessionNo, n.Status, n.Headline, raw, n.PreviousAccessionNo, n.Reason, n.Model, n.PromptVersion)
	if err != nil {
		return fmt.Errorf("save note of %s: %w", n.AccessionNo, err)
	}
	return nil
}

// NotesToWrite lists high-level 8-Ks that have details and no note row,
// newest first, at most limit. A rejected note is a row too, so a filing the
// model failed on is not retried every cycle.
func (s *Store) NotesToWrite(ctx context.Context, limit int) ([]NoteTask, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT f.accession_no, f.form_type, f.cik, f.company, d.items, d.primary_doc, f.accepted_at
		FROM filings f
		JOIN triage t ON t.accession_no = f.accession_no
		JOIN filing_details d ON d.accession_no = f.accession_no
		LEFT JOIN notes n ON n.accession_no = f.accession_no
		WHERE f.form_type IN ('8-K', '8-K/A') AND t.level = 'high' AND d.primary_doc <> '' AND n.accession_no IS NULL
		ORDER BY f.accepted_at DESC, f.accession_no DESC
		LIMIT $1`,
		limit)
	if err != nil {
		return nil, fmt.Errorf("list notes to write: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (NoteTask, error) {
		var n NoteTask
		err := row.Scan(&n.AccessionNo, &n.FormType, &n.CIK, &n.Company, &n.Items, &n.PrimaryDoc, &n.AcceptedAt)
		n.Items = orEmpty(n.Items)
		return n, err
	})
	if err != nil {
		return nil, fmt.Errorf("list notes to write: %w", err)
	}
	return out, nil
}

// NotesToday counts notes created since midnight UTC, published or not.
func (s *Store) NotesToday(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM notes
		WHERE created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count notes: %w", err)
	}
	return n, nil
}
