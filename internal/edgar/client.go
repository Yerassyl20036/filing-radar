package edgar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is EDGAR's website.
const DefaultBaseURL = "https://www.sec.gov"

// PageSize is the most entries EDGAR returns per request.
const PageSize = 100

// A page is about 60 KB; anything far bigger isn't the feed.
const maxBody = 10 << 20

// Client fetches the latest-filings feed politely: it names its operator, as
// SEC requires, and leaves at least minGap between requests. SEC's limit is
// 10 requests per second; one per second keeps us far below it.
type Client struct {
	baseURL   string
	userAgent string
	minGap    time.Duration
	http      *http.Client

	mu   sync.Mutex
	last time.Time // when the last request was sent
}

// NewClient checks the User-Agent up front: without a contact address SEC
// answers 403 to every request, so there's no point starting.
func NewClient(baseURL, userAgent string, minGap time.Duration) (*Client, error) {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return nil, errors.New("EDGAR_USER_AGENT is not set")
	}
	if !strings.Contains(userAgent, "@") {
		return nil, errors.New("EDGAR_USER_AGENT must include a contact email, as SEC requires")
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		userAgent: userAgent,
		minGap:    minGap,
		http:      &http.Client{Timeout: 20 * time.Second},
	}, nil
}

// StatusError is a response from SEC other than 200 OK.
type StatusError struct{ Code int }

func (e *StatusError) Error() string {
	switch e.Code {
	case http.StatusForbidden:
		return "SEC answered 403: an undeclared User-Agent, or over the rate limit"
	case http.StatusTooManyRequests:
		return "SEC answered 429: too many requests"
	}
	return fmt.Sprintf("SEC answered %d", e.Code)
}

// Latest fetches one page of the newest filings, starting at entry start
// (0, 100, 200…).
func (c *Client) Latest(ctx context.Context, start int) (Feed, error) {
	if err := c.wait(ctx); err != nil {
		return Feed{}, err
	}

	q := url.Values{
		"action":  {"getcurrent"},
		"type":    {""},
		"company": {""},
		"dateb":   {""},
		"owner":   {"include"},
		"start":   {strconv.Itoa(start)},
		"count":   {strconv.Itoa(PageSize)},
		"output":  {"atom"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/cgi-bin/browse-edgar?"+q.Encode(), nil)
	if err != nil {
		return Feed{}, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/atom+xml")

	resp, err := c.http.Do(req)
	if err != nil {
		return Feed{}, fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // lets the connection be reused
		return Feed{}, &StatusError{Code: resp.StatusCode}
	}
	return ParseFeed(io.LimitReader(resp.Body, maxBody))
}

// wait blocks until minGap has passed since the previous request. Holding the
// lock while waiting makes concurrent callers queue up instead of bursting.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := time.Until(c.last.Add(c.minGap)); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	c.last = time.Now()
	return nil
}
