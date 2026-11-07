package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The pages need no database, so they can be checked without one.
func TestPages(t *testing.T) {
	mux := http.NewServeMux()
	routePages(mux)

	tests := []struct {
		path, contentType, contains string
	}{
		{"/", "text/html", "Latest SEC filings"},
		{"/status", "text/html", "System status"},
		{"/how-it-works", "text/html", "What happens to one filing"},
		{"/how-it-works", "text/html", `id="costs"`},
		{"/how-it-works", "text/html", `id="scorecard"`},
		{"/how-it-works", "text/html", "/api/scorecard"},
		{"/", "text/html", "next close: "},
		{"/", "text/html", "/how-it-works#costs"},
		{"/about", "text/html", "How it was built"},
		{"/about", "text/html", "https://github.com/Yerassyl20036"},
		{"/status", "text/html", `href="/about"`},
		{"/static/site.css", "text/css", "--up"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", tt.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tt.contentType) {
			t.Errorf("%s: content type %q, want %s", tt.path, ct, tt.contentType)
		}
		if !strings.Contains(rec.Body.String(), tt.contains) {
			t.Errorf("%s: body doesn't contain %q", tt.path, tt.contains)
		}
	}
}
