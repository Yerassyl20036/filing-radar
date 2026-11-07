package prices

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is Yahoo's chart API. Checked in November 2026: it answers
// without a key and returns daily closes with timestamps; Stooq's CSV now
// sits behind a JavaScript challenge.
const DefaultBaseURL = "https://query1.finance.yahoo.com"

// userAgent names the project only: the API needs no contact, so no email
// is sent anywhere.
const userAgent = "FilingRadar/1.0"

// A month of daily bars is a few KB.
const maxBody = 4 << 20

// Yahoo reads daily closes from Yahoo's chart API, at most one request per
// MinGap.
type Yahoo struct {
	BaseURL string        // DefaultBaseURL unless testing
	MinGap  time.Duration // the least time between two requests
	// Now says what time it is; bars whose close is still ahead are the
	// day's live price, not a close. Nil means time.Now.
	Now func() time.Time

	http *http.Client
	mu   sync.Mutex
	last time.Time
}

// NewYahoo returns a client for baseURL ("" means DefaultBaseURL) that waits
// a second between requests.
func NewYahoo(baseURL string) *Yahoo {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Yahoo{BaseURL: strings.TrimRight(baseURL, "/"), MinGap: time.Second, http: &http.Client{Timeout: 20 * time.Second}}
}

// StatusError is an answer other than 200 or 404.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("price source answered %d", e.Code) }

// Closes asks for daily bars between from and to and keeps the finished ones.
func (y *Yahoo) Closes(ctx context.Context, ticker string, from, to time.Time) ([]Close, error) {
	if err := y.wait(ctx); err != nil {
		return nil, err
	}
	q := url.Values{
		"interval": {"1d"},
		"period1":  {strconv.FormatInt(from.Unix(), 10)},
		"period2":  {strconv.FormatInt(to.Unix(), 10)},
	}
	u := y.BaseURL + "/v8/finance/chart/" + url.PathEscape(ticker) + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	hc := y.http
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch prices of %s: %w", ticker, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", ticker, ErrUnknownTicker)
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &StatusError{Code: resp.StatusCode}
	}
	now := time.Now()
	if y.Now != nil {
		now = y.Now()
	}
	closes, err := parseChart(io.LimitReader(resp.Body, maxBody), now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ticker, err)
	}
	return closes, nil
}

// parseChart reads the chart API's answer. Each bar's timestamp is that
// day's open; the close is closeOn the same day (16:00 New York, or 13:00
// on an early close). Bars with no close
// (a halt) or whose close is still ahead of now are left out.
func parseChart(r io.Reader, now time.Time) ([]Close, error) {
	var body struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Quote []struct {
						Close []*float64 `json:"close"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
			Error *struct {
				Code        string `json:"code"`
				Description string `json:"description"`
			} `json:"error"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, fmt.Errorf("parse prices: %w", err)
	}
	if e := body.Chart.Error; e != nil {
		if e.Code == "Not Found" {
			return nil, ErrUnknownTicker
		}
		return nil, fmt.Errorf("price source: %s: %s", e.Code, e.Description)
	}
	if len(body.Chart.Result) == 0 {
		return nil, nil
	}
	res := body.Chart.Result[0]
	if len(res.Indicators.Quote) == 0 {
		return nil, nil
	}
	closes := res.Indicators.Quote[0].Close
	var out []Close
	for i, ts := range res.Timestamp {
		if i >= len(closes) || closes[i] == nil || *closes[i] <= 0 {
			continue
		}
		day := time.Unix(ts, 0).In(NewYork)
		at := closeOn(day.Year(), day.Month(), day.Day())
		if at.After(now) {
			continue
		}
		// The API sends float32 noise (579.0900268554688); cents and a bit are plenty.
		price := math.Round(*closes[i]*10000) / 10000
		out = append(out, Close{Day: at.Format("2006-01-02"), At: at, Price: price})
	}
	return out, nil
}

// wait blocks until MinGap has passed since the previous request.
func (y *Yahoo) wait(ctx context.Context) error {
	y.mu.Lock()
	defer y.mu.Unlock()
	if d := time.Until(y.last.Add(y.MinGap)); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	y.last = time.Now()
	return nil
}
