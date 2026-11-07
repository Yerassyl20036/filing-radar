package edgar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tickersJSON = `{
	"0":{"cik_str":320193,"ticker":"AAPL","title":"Apple Inc."},
	"2":{"cik_str":1652044,"ticker":"GOOG","title":"Alphabet Inc."},
	"1":{"cik_str":1652044,"ticker":"GOOGL","title":"Alphabet Inc."},
	"3":{"cik_str":1067983,"ticker":"brk-b","title":"BERKSHIRE HATHAWAY INC"},
	"4":{"cik_str":0,"ticker":"BAD","title":"no CIK"}
}`

func TestParseTickers(t *testing.T) {
	got, err := ParseTickers(strings.NewReader(tickersJSON))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"0000320193": "AAPL", "0001652044": "GOOGL", "0001067983": "BRK-B"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for cik, tk := range want {
		if got[cik] != tk {
			t.Errorf("%s: got %q, want %q", cik, got[cik], tk)
		}
	}
}

func TestParseTickersRejectsJunk(t *testing.T) {
	for _, in := range []string{"", "[]", "{}", "<html>"} {
		if _, err := ParseTickers(strings.NewReader(in)); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestTickers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/files/company_tickers.json" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if got := r.Header.Get("User-Agent"); got != testAgent {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = w.Write([]byte(tickersJSON))
	}))
	defer srv.Close()

	c, _ := NewClient(Config{BaseURL: srv.URL, UserAgent: testAgent})
	got, err := c.Tickers(context.Background())
	if err != nil || got["0000320193"] != "AAPL" {
		t.Fatalf("got %v, %v", got, err)
	}
}
