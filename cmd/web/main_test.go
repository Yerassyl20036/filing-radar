package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
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
		{"/static/logo.svg", "image/svg+xml", "<svg"},
		{"/static/apple-touch-icon.png", "image/png", "PNG"},
		{"/static/logos/go.svg", "image/svg+xml", "<svg"},
		{"/static/logos/postgresql.svg", "image/svg+xml", "<svg"},
		{"/static/logos/temporal.svg", "image/svg+xml", "<svg"},
		{"/static/logos/deepseek.svg", "image/svg+xml", "<svg"},
		{"/static/logos/claude.svg", "image/svg+xml", "<svg"},
		{"/static/logos/NOTICE.txt", "text/plain", "CC0"},
		{"/", "text/html", `rel="icon" type="image/svg+xml" href="/static/logo.svg"`},
		{"/status", "text/html", `href="/static/logo.svg"`},
		{"/how-it-works", "text/html", "no affiliation or sponsorship"},
		{"/about", "text/html", "no affiliation or sponsorship"},
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

// Every logo the stylesheet masks with must be embedded, or it renders as a blank square.
func TestLogoFilesExist(t *testing.T) {
	css, err := static.ReadFile("static/site.css")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`url\("(logos/[a-z]+\.svg)"\)`).FindAllStringSubmatch(string(css), -1)
	if len(refs) < 5 {
		t.Fatalf("site.css references %d logos, want at least 5", len(refs))
	}
	for _, m := range refs {
		if _, err := fs.Stat(static, "static/"+m[1]); err != nil {
			t.Errorf("site.css uses %s, but it isn't embedded: %v", m[1], err)
		}
	}
}
