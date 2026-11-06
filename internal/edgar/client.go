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

const (
	// DefaultBaseURL is EDGAR's website: the feed and the filings themselves.
	DefaultBaseURL = "https://www.sec.gov"
	// DefaultDataURL hosts SEC's JSON APIs.
	DefaultDataURL = "https://data.sec.gov"
)

// PageSize is the most entries EDGAR returns per request.
const PageSize = 100

// A feed page is about 60 KB and a company's JSON a few MB at most; anything
// far bigger isn't what we asked for.
const maxBody = 32 << 20

// Config says where SEC lives and who we are.
type Config struct {
	BaseURL   string        // DefaultBaseURL unless testing
	DataURL   string        // DefaultDataURL unless testing
	UserAgent string        // a name and contact email, as SEC requires
	MinGap    time.Duration // the least time between two requests
}

// Client talks to SEC politely: it names its operator, as SEC requires, and
// leaves at least MinGap between requests to either host. SEC's limit is 10
// requests per second; one per second keeps us far below it. The poller and
// the triage workflows share one Client, so they share that budget.
type Client struct {
	cfg  Config
	http *http.Client

	mu   sync.Mutex
	last time.Time // when the last request was sent
}

// NewClient checks the User-Agent up front: without a contact address SEC
// answers 403 to every request, so there's no point starting.
func NewClient(cfg Config) (*Client, error) {
	cfg.UserAgent = strings.TrimSpace(cfg.UserAgent)
	if cfg.UserAgent == "" {
		return nil, errors.New("EDGAR_USER_AGENT is not set")
	}
	if !strings.Contains(cfg.UserAgent, "@") {
		return nil, errors.New("EDGAR_USER_AGENT must include a contact email, as SEC requires")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.DataURL == "" {
		cfg.DataURL = DefaultDataURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.DataURL = strings.TrimRight(cfg.DataURL, "/")
	return &Client{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}}, nil
}

// StatusError is a response from SEC other than 200 OK.
type StatusError struct{ Code int }

func (e *StatusError) Error() string {
	switch e.Code {
	case http.StatusForbidden:
		return "SEC answered 403: an undeclared User-Agent, or over the rate limit"
	case http.StatusNotFound:
		return "SEC answered 404: no such page"
	case http.StatusTooManyRequests:
		return "SEC answered 429: too many requests"
	}
	return fmt.Sprintf("SEC answered %d", e.Code)
}

// Latest fetches one page of the newest filings, starting at entry start
// (0, 100, 200…).
func (c *Client) Latest(ctx context.Context, start int) (Feed, error) {
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
	body, err := c.get(ctx, c.cfg.BaseURL+"/cgi-bin/browse-edgar?"+q.Encode(), "application/atom+xml")
	if err != nil {
		return Feed{}, err
	}
	defer body.Close()
	return ParseFeed(io.LimitReader(body, maxBody))
}

// get sends one GET, after waiting its turn. The caller closes the body.
func (c *Client) get(ctx context.Context, url, accept string) (io.ReadCloser, error) {
	resp, err := c.fetch(ctx, url, accept)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// fetch is get for callers that also need the response headers, such as a
// document's Content-Type. The caller closes the body.
func (c *Client) fetch(ctx context.Context, url, accept string) (*http.Response, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", accept)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch from SEC: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // lets the connection be reused
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode}
	}
	return resp, nil
}

// wait blocks until MinGap has passed since the previous request. Holding the
// lock while waiting makes concurrent callers queue up instead of bursting.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := time.Until(c.last.Add(c.cfg.MinGap)); d > 0 {
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
