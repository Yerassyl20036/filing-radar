package edgar

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

const testAgent = "Filing Radar test@example.com"

func TestLatest(t *testing.T) {
	page, err := os.ReadFile("testdata/latest.atom")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/cgi-bin/browse-edgar" || q.Get("action") != "getcurrent" || q.Get("output") != "atom" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if q.Get("start") != "100" || q.Get("count") != "100" {
			t.Errorf("start=%s count=%s, want 100 and 100", q.Get("start"), q.Get("count"))
		}
		if got := r.Header.Get("User-Agent"); got != testAgent {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = w.Write(page)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, UserAgent: testAgent})
	if err != nil {
		t.Fatal(err)
	}
	feed, err := c.Latest(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Entries) != 100 {
		t.Errorf("got %d entries", len(feed.Entries))
	}
}

func TestLatestReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c, _ := NewClient(Config{BaseURL: srv.URL, UserAgent: testAgent})
	_, err := c.Latest(context.Background(), 0)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want a 429 StatusError", err)
	}
}

func TestLatestKeepsItsDistance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"></feed>`))
	}))
	defer srv.Close()

	const gap = 80 * time.Millisecond
	c, _ := NewClient(Config{BaseURL: srv.URL, UserAgent: testAgent, MinGap: gap})
	start := time.Now()
	for range 3 {
		if _, err := c.Latest(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took < 2*gap {
		t.Errorf("3 requests took %v, want at least %v", took, 2*gap)
	}
}

func TestNewClientWantsAContact(t *testing.T) {
	for _, ua := range []string{"", "Filing Radar", "Go-http-client/1.1"} {
		if _, err := NewClient(Config{UserAgent: ua}); err == nil {
			t.Errorf("User-Agent %q was accepted", ua)
		}
	}
}
