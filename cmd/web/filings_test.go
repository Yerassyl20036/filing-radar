package main

import (
	"net/http/httptest"
	"slices"
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
		query      string
		wantErr    bool
		wantLimit  int
		wantForm   string
		wantLevels []string // nil means every filing, pending included
	}{
		{"", false, 50, "", nil},
		{"?form=8-K&limit=10", false, 10, "8-K", nil},
		{"?before=" + good, false, 50, "", nil},
		{"?level=high", false, 50, "", []string{"high"}},
		{"?level=medium&form=8-K", false, 50, "8-K", []string{"high", "medium"}},
		{"?level=low", false, 50, "", []string{"high", "medium", "low"}},
		{"?level=routine", false, 50, "", []string{"high", "medium", "low", "routine"}},
		{"?level=", false, 50, "", nil},
		{"?level=urgent", true, 0, "", nil},
		{"?level=HIGH", true, 0, "", nil},
		{"?limit=0", true, 0, "", nil},
		{"?limit=500", true, 0, "", nil},
		{"?limit=ten", true, 0, "", nil},
		{"?before=not-a-cursor", true, 0, "", nil},
	}
	for _, tt := range tests {
		q, err := parseFilingQuery(httptest.NewRequest("GET", "/api/filings"+tt.query, nil))
		if (err != nil) != tt.wantErr {
			t.Errorf("%q: err = %v", tt.query, err)
			continue
		}
		if tt.wantErr {
			continue
		}
		if q.Limit != tt.wantLimit || q.FormType != tt.wantForm {
			t.Errorf("%q: limit %d form %q", tt.query, q.Limit, q.FormType)
		}
		if !slices.Equal(q.Levels, tt.wantLevels) || (q.Levels == nil) != (tt.wantLevels == nil) {
			t.Errorf("%q: levels %q, want %q", tt.query, q.Levels, tt.wantLevels)
		}
	}
}
