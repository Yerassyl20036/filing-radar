package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Yerassyl20036/filing-radar/internal/store"
)

func TestCursorRoundTrip(t *testing.T) {
	in := store.Cursor{AcceptedAt: time.Date(2026, 11, 6, 2, 53, 57, 123456000, time.UTC), AccessionNo: "0001193125-26-450499"}
	out, err := decodeCursor(encodeCursor(in))
	if err != nil {
		t.Fatal(err)
	}
	if !out.AcceptedAt.Equal(in.AcceptedAt) || out.AccessionNo != in.AccessionNo {
		t.Errorf("got %+v, want %+v", out, in)
	}
}

func TestParseFilingQuery(t *testing.T) {
	good := encodeCursor(store.Cursor{AcceptedAt: time.Now(), AccessionNo: "0001193125-26-450499"})
	tests := []struct {
		query     string
		wantErr   bool
		wantLimit int
		wantForm  string
	}{
		{"", false, 50, ""},
		{"?form=8-K&limit=10", false, 10, "8-K"},
		{"?before=" + good, false, 50, ""},
		{"?limit=0", true, 0, ""},
		{"?limit=500", true, 0, ""},
		{"?limit=ten", true, 0, ""},
		{"?before=not-a-cursor", true, 0, ""},
	}
	for _, tt := range tests {
		q, err := parseFilingQuery(httptest.NewRequest("GET", "/api/filings"+tt.query, nil))
		if (err != nil) != tt.wantErr {
			t.Errorf("%q: err = %v", tt.query, err)
			continue
		}
		if !tt.wantErr && (q.Limit != tt.wantLimit || q.FormType != tt.wantForm) {
			t.Errorf("%q: limit %d form %q", tt.query, q.Limit, q.FormType)
		}
	}
}
