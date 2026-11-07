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
	Triage      *Triage         `json:"triage"`  // nil while pending
	Note        *Note           `json:"note"`    // only a published note; nil otherwise
	Outcome     *Outcome        `json:"outcome"` // only a scored outcome; nil otherwise
}

// Cursor marks the last row of a page; the next page starts after it.
// accession_no breaks ties: many filings share the same second.
type Cursor struct {
	AcceptedAt  time.Time
	AccessionNo string
}

type FilingQuery struct {
	FormType string   // "" for every form
	Levels   []string // only filings labelled one of these; nil for all, pending included
	Limit    int      // at most this many rows
	Before   *Cursor  // nil for the newest
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
		where = append(where, "f.form_type = "+arg(q.FormType))
	}
	if q.Levels != nil {
		// A pending filing has no triage row, so its NULL level never matches.
		where = append(where, "t.level = ANY("+arg(q.Levels)+")")
	}
	if q.Before != nil {
		where = append(where, fmt.Sprintf("(f.accepted_at, f.accession_no) < (%s, %s)", arg(q.Before.AcceptedAt), arg(q.Before.AccessionNo)))
	}

	sql := `SELECT f.accession_no, f.form_type, f.cik, f.company, f.accepted_at, f.url, f.parties, f.first_seen_at,
		t.level, t.reasons, t.rules_version, t.model_score, t.model_reason, t.model_quote, t.model, t.decided_at, d.items,
		n.status, n.headline, n.claims, n.previous_accession_no, n.reason, n.model, n.prompt_version, n.created_at,
		o.ticker, o.prev_close::float8, o.next_close::float8, o.move_pct::float8,
		to_char(o.prev_date, 'YYYY-MM-DD'), to_char(o.next_date, 'YYYY-MM-DD'), o.created_at
		FROM filings f
		LEFT JOIN triage t ON t.accession_no = f.accession_no
		LEFT JOIN filing_details d ON d.accession_no = f.accession_no
		LEFT JOIN notes n ON n.accession_no = f.accession_no AND n.status = 'published'
		LEFT JOIN outcomes o ON o.accession_no = f.accession_no AND o.status = 'scored'`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY f.accepted_at DESC, f.accession_no DESC LIMIT " + arg(q.Limit)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read filings: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanFilingRow)
	if err != nil {
		return nil, fmt.Errorf("read filings: %w", err)
	}
	return out, nil
}

// scanFilingRow reads one row of Filings. The triage columns are all NULL
// when the filing is still pending, and the note columns when there is no
// published note, and the outcome columns when there is no scored outcome,
// so they land in nullable values first.
func scanFilingRow(row pgx.CollectableRow) (FilingRow, error) {
	var (
		f            FilingRow
		level        *string
		reasons      []string
		rulesVersion *int
		modelScore   *int
		modelReason  *string
		modelQuote   *string
		model        *string
		decidedAt    *time.Time
		items        []string
		noteStatus   *string
		headline     *string
		claims       []byte
		previous     *string
		noteReason   *string
		noteModel    *string
		promptVer    *int
		noteAt       *time.Time
		ticker       *string
		prevClose    *float64
		nextClose    *float64
		movePct      *float64
		prevDate     *string
		nextDate     *string
		outcomeAt    *time.Time
	)
	err := row.Scan(&f.AccessionNo, &f.FormType, &f.CIK, &f.Company, &f.AcceptedAt, &f.URL, &f.Parties, &f.FirstSeenAt,
		&level, &reasons, &rulesVersion, &modelScore, &modelReason, &modelQuote, &model, &decidedAt, &items,
		&noteStatus, &headline, &claims, &previous, &noteReason, &noteModel, &promptVer, &noteAt,
		&ticker, &prevClose, &nextClose, &movePct, &prevDate, &nextDate, &outcomeAt)
	if err != nil {
		return f, err
	}
	if ticker != nil && movePct != nil && prevClose != nil && nextClose != nil && prevDate != nil && nextDate != nil {
		f.Outcome = &Outcome{
			AccessionNo: f.AccessionNo, Status: OutcomeScored, Ticker: *ticker,
			PrevClose: *prevClose, NextClose: *nextClose, MovePct: *movePct,
			PrevDate: *prevDate, NextDate: *nextDate, CreatedAt: *outcomeAt,
		}
	}
	if noteStatus != nil {
		n := &Note{
			AccessionNo:         f.AccessionNo,
			Status:              *noteStatus,
			Headline:            *headline,
			PreviousAccessionNo: *previous,
			Reason:              *noteReason,
			Model:               *noteModel,
			PromptVersion:       *promptVer,
			CreatedAt:           *noteAt,
		}
		if err := json.Unmarshal(claims, &n.Claims); err != nil {
			return f, fmt.Errorf("decode claims of %s: %w", f.AccessionNo, err)
		}
		if n.Claims == nil {
			n.Claims = []Claim{} // the API promises an array
		}
		f.Note = n
	}
	if level == nil {
		return f, nil
	}
	f.Triage = &Triage{
		AccessionNo:  f.AccessionNo,
		Level:        *level,
		Reasons:      orEmpty(reasons),
		Items:        orEmpty(items),
		RulesVersion: *rulesVersion,
		ModelScore:   modelScore,
		ModelReason:  *modelReason,
		ModelQuote:   *modelQuote,
		Model:        *model,
		DecidedAt:    *decidedAt,
	}
	return f, nil
}
