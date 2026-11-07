package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outcome statuses, as stored.
const (
	OutcomeScored   = "scored"
	OutcomeNoTicker = "no ticker"
	OutcomeNoPrice  = "no price"
)

// BigMovePct is the move, either way, that the scorecard counts as big.
const BigMovePct = 3.0

// Outcome is what the stock did after one scored 8-K. Prices and dates are
// set only when Status is "scored".
type Outcome struct {
	AccessionNo string    `json:"-"`
	Status      string    `json:"status"`
	Ticker      string    `json:"ticker,omitempty"`
	PrevClose   float64   `json:"prev_close,omitempty"`
	NextClose   float64   `json:"next_close,omitempty"`
	MovePct     float64   `json:"move_pct"`
	PrevDate    string    `json:"prev_date,omitempty"` // "2006-01-02", New York trading day
	NextDate    string    `json:"next_date,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// OutcomeTask is a scored 8-K that has no outcome yet.
type OutcomeTask struct {
	AccessionNo string
	CIK         string
	Company     string
	AcceptedAt  time.Time
}

// SaveOutcome stores an outcome. Saving again overwrites, so a retry is
// harmless.
func (s *Store) SaveOutcome(ctx context.Context, o Outcome) error {
	var prev, next, move, prevDate, nextDate any // NULL unless scored
	if o.Status == OutcomeScored {
		prev, next, move, prevDate, nextDate = o.PrevClose, o.NextClose, o.MovePct, o.PrevDate, o.NextDate
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO outcomes (accession_no, status, ticker, prev_close, next_close, move_pct, prev_date, next_date, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::date, $8::date, now())
		ON CONFLICT (accession_no) DO UPDATE
		SET status = EXCLUDED.status, ticker = EXCLUDED.ticker, prev_close = EXCLUDED.prev_close,
			next_close = EXCLUDED.next_close, move_pct = EXCLUDED.move_pct, prev_date = EXCLUDED.prev_date,
			next_date = EXCLUDED.next_date, created_at = EXCLUDED.created_at`,
		o.AccessionNo, o.Status, o.Ticker, prev, next, move, prevDate, nextDate)
	if err != nil {
		return fmt.Errorf("save outcome of %s: %w", o.AccessionNo, err)
	}
	return nil
}

// OutcomesToTrack lists 8-Ks with a model score and no outcome row, oldest
// first, at most limit. Oldest first: a workflow still waiting for its close
// is listed until it ends, and new filings must not push old ones out.
func (s *Store) OutcomesToTrack(ctx context.Context, limit int) ([]OutcomeTask, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT f.accession_no, f.cik, f.company, f.accepted_at
		FROM filings f
		JOIN triage t ON t.accession_no = f.accession_no
		LEFT JOIN outcomes o ON o.accession_no = f.accession_no
		WHERE f.form_type IN ('8-K', '8-K/A') AND t.model_score IS NOT NULL AND o.accession_no IS NULL
		ORDER BY f.accepted_at, f.accession_no
		LIMIT $1`,
		limit)
	if err != nil {
		return nil, fmt.Errorf("list outcomes to track: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OutcomeTask, error) {
		var o OutcomeTask
		err := row.Scan(&o.AccessionNo, &o.CIK, &o.Company, &o.AcceptedAt)
		return o, err
	})
	if err != nil {
		return nil, fmt.Errorf("list outcomes to track: %w", err)
	}
	return out, nil
}

// Bucket is one row of the scorecard: the scored outcomes in one group.
// The means are nil when the group has none.
type Bucket struct {
	Label        string   `json:"label"`
	Scored       int      `json:"scored"`
	MeanAbsMove  *float64 `json:"mean_abs_move_pct"`
	ShareBigMove *float64 `json:"share_big_move"` // 0 to 1: moves of BigMovePct or more, either way
}

// Scorecard is stage 4's eval: did higher model scores, and higher rule
// levels, come with bigger moves?
type Scorecard struct {
	ByScore    []Bucket   `json:"by_score"`
	ByLevel    []Bucket   `json:"by_level"`
	Scored     int        `json:"scored"`
	NoTicker   int        `json:"no_ticker"`
	NoPrice    int        `json:"no_price"`
	BigMovePct float64    `json:"big_move_pct"`
	Since      *time.Time `json:"since"` // the first outcome; nil when there are none
}

// Scorecard sums the outcomes. Every bucket is listed, empty or not, so the
// table's shape never changes.
func (s *Store) Scorecard(ctx context.Context) (Scorecard, error) {
	sc := Scorecard{BigMovePct: BigMovePct, ByScore: []Bucket{}, ByLevel: []Bucket{}}
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'scored'), count(*) FILTER (WHERE status = 'no ticker'),
		       count(*) FILTER (WHERE status = 'no price'), min(created_at)
		FROM outcomes`).Scan(&sc.Scored, &sc.NoTicker, &sc.NoPrice, &sc.Since)
	if err != nil {
		return Scorecard{}, fmt.Errorf("count outcomes: %w", err)
	}
	if sc.Since != nil {
		*sc.Since = sc.Since.UTC()
	}

	// The level is the label's current one. The score bands are the
	// prompt's: 70 and up is "likely to move the price", 40 to 69 notable
	// news, and the rules raise a label at the same lines.
	const scored = `SELECT t.model_score, t.level, abs(o.move_pct)::float8 AS m
		FROM outcomes o JOIN triage t ON t.accession_no = o.accession_no
		WHERE o.status = 'scored'`
	groups := []struct {
		into *[]Bucket
		sql  string
	}{
		{&sc.ByScore, `WITH s AS (` + scored + `)
			SELECT b.label, count(s.m), avg(s.m), avg((s.m >= $1)::int)::float8
			FROM (VALUES ('0-39', 0, 39, 1), ('40-69', 40, 69, 2), ('70-100', 70, 100, 3)) b(label, lo, hi, ord)
			LEFT JOIN s ON s.model_score BETWEEN b.lo AND b.hi
			GROUP BY b.label, b.ord ORDER BY b.ord`},
		{&sc.ByLevel, `WITH s AS (` + scored + `)
			SELECT b.label, count(s.m), avg(s.m), avg((s.m >= $1)::int)::float8
			FROM (VALUES ('high', 1), ('medium', 2), ('low', 3)) b(label, ord)
			LEFT JOIN s ON s.level = b.label
			GROUP BY b.label, b.ord ORDER BY b.ord`},
	}
	for _, g := range groups {
		rows, err := s.pool.Query(ctx, g.sql, BigMovePct)
		if err != nil {
			return Scorecard{}, fmt.Errorf("sum outcomes: %w", err)
		}
		b, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Bucket, error) {
			var b Bucket
			err := row.Scan(&b.Label, &b.Scored, &b.MeanAbsMove, &b.ShareBigMove)
			return b, err
		})
		if err != nil {
			return Scorecard{}, fmt.Errorf("sum outcomes: %w", err)
		}
		*g.into = b
	}
	return sc, nil
}
