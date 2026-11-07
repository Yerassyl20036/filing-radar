package store

import (
	"context"
	"fmt"
	"time"
)

// Spend sums model calls over a period.
type Spend struct {
	USD          float64    `json:"usd"`
	Calls        int        `json:"calls"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	Since        *time.Time `json:"since"` // the first call in the period; nil when there are none
}

// PurposeSpend is all-time spend for one kind of call.
type PurposeSpend struct {
	Purpose string  `json:"purpose"`
	USD     float64 `json:"usd"`
	Calls   int     `json:"calls"`
}

// Costs is what the model has cost, for the "What it costs" section.
type Costs struct {
	AllTime   Spend          `json:"all_time"`
	Today     Spend          `json:"today"`
	ByPurpose []PurposeSpend `json:"by_purpose"`
}

// ModelCosts sums every recorded model call: all time, since midnight UTC,
// and by purpose, most expensive first.
func (s *Store) ModelCosts(ctx context.Context) (Costs, error) {
	var c Costs
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(sum(cost_usd), 0)::float8, count(*),
		       coalesce(sum(input_tokens), 0)::int8, coalesce(sum(output_tokens), 0)::int8, min(created_at),
		       coalesce(sum(cost_usd) FILTER (WHERE today), 0)::float8, count(*) FILTER (WHERE today),
		       coalesce(sum(input_tokens) FILTER (WHERE today), 0)::int8, coalesce(sum(output_tokens) FILTER (WHERE today), 0)::int8,
		       min(created_at) FILTER (WHERE today)
		FROM (
			SELECT *, created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS today
			FROM model_calls
		) calls`).Scan(
		&c.AllTime.USD, &c.AllTime.Calls, &c.AllTime.InputTokens, &c.AllTime.OutputTokens, &c.AllTime.Since,
		&c.Today.USD, &c.Today.Calls, &c.Today.InputTokens, &c.Today.OutputTokens, &c.Today.Since)
	if err != nil {
		return Costs{}, fmt.Errorf("sum model costs: %w", err)
	}
	for _, t := range []*time.Time{c.AllTime.Since, c.Today.Since} {
		if t != nil {
			*t = t.UTC()
		}
	}

	rows, err := s.pool.Query(ctx, `
		SELECT purpose, coalesce(sum(cost_usd), 0)::float8, count(*)
		FROM model_calls GROUP BY purpose ORDER BY 2 DESC, 1`)
	if err != nil {
		return Costs{}, fmt.Errorf("sum model costs by purpose: %w", err)
	}
	defer rows.Close()
	c.ByPurpose = []PurposeSpend{} // [] in JSON, not null
	for rows.Next() {
		var p PurposeSpend
		if err := rows.Scan(&p.Purpose, &p.USD, &p.Calls); err != nil {
			return Costs{}, fmt.Errorf("read model costs by purpose: %w", err)
		}
		c.ByPurpose = append(c.ByPurpose, p)
	}
	if err := rows.Err(); err != nil {
		return Costs{}, fmt.Errorf("read model costs by purpose: %w", err)
	}
	return c, nil
}
