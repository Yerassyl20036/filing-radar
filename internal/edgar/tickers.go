package edgar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Tickers fetches SEC's list of companies with a ticker and returns each
// company's ticker by its 10-digit CIK. A company with several share classes
// (GOOGL and GOOG) is listed once per ticker; the first listing wins, which
// SEC orders by size, so it's the main class.
func (c *Client) Tickers(ctx context.Context) (map[string]string, error) {
	body, err := c.get(ctx, c.cfg.BaseURL+"/files/company_tickers.json", "application/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return ParseTickers(io.LimitReader(body, maxBody))
}

// ParseTickers reads company_tickers.json: {"0":{"cik_str":320193,"ticker":"AAPL","title":"Apple Inc."},...}.
func ParseTickers(r io.Reader) (map[string]string, error) {
	var raw map[string]struct {
		CIK    int64  `json:"cik_str"`
		Ticker string `json:"ticker"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse company tickers: %w", err)
	}
	// The keys are ranks as strings; a map has no order, so keep the
	// lowest rank per company by hand.
	out := make(map[string]string, len(raw))
	rank := make(map[string]int, len(raw))
	for key, e := range raw {
		n, err := strconv.Atoi(key)
		ticker := strings.ToUpper(strings.TrimSpace(e.Ticker))
		if err != nil || e.CIK <= 0 || ticker == "" {
			continue
		}
		cik := fmt.Sprintf("%010d", e.CIK)
		if r, seen := rank[cik]; seen && r <= n {
			continue
		}
		out[cik], rank[cik] = ticker, n
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("parse company tickers: no companies listed")
	}
	return out, nil
}
