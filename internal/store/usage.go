package store

import (
	"context"
	"fmt"
)

// ModelCall is one request to a language model, with what it cost.
type ModelCall struct {
	AccessionNo  string
	Purpose      string // "score", "draft" or "repair"
	Provider     string // "deepseek", "gemini", "groq" or "anthropic"
	Model        string // as the API reported it
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// RecordModelCall stores one call.
func (s *Store) RecordModelCall(ctx context.Context, c ModelCall) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO model_calls (accession_no, purpose, provider, model, input_tokens, output_tokens, cost_usd)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		c.AccessionNo, c.Purpose, c.Provider, c.Model, c.InputTokens, c.OutputTokens, c.CostUSD)
	if err != nil {
		return fmt.Errorf("record model call: %w", err)
	}
	return nil
}

// ModelSpendToday sums calls since midnight UTC, so the daily budget holds
// across restarts.
func (s *Store) ModelSpendToday(ctx context.Context) (usd float64, calls int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT coalesce(sum(cost_usd), 0)::float8, count(*) FROM model_calls
		WHERE created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&usd, &calls)
	if err != nil {
		return 0, 0, fmt.Errorf("sum model spend: %w", err)
	}
	return usd, calls, nil
}
