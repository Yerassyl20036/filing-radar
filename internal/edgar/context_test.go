package edgar

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// A company's filings newest first: two quarterly earnings releases with a
// leadership change and other forms in between.
const historySubmissions = `{
  "cik": "320193", "name": "Apple Inc.",
  "filings": {"recent": {
    "accessionNumber": ["0000320193-26-000300", "0000320193-26-000250", "0000320193-26-000240", "0000320193-26-000200", "0000320193-26-000150", "0000320193-26-000100"],
    "form":            ["8-K",                  "8-K",                  "10-Q",                 "8-K/A",                "4",                    "8-K"],
    "items":           ["2.02,9.01",            "5.02",                 "",                     "2.02, 9.01",           "",                     "5.02,9.01"],
    "primaryDocument": ["q4.htm",               "ceo.htm",              "10q.htm",              "q3a.htm",              "form4.xml",            "cfo.htm"],
    "filingDate":      ["2026-10-30",           "2026-09-15",           "2026-08-01",           "2026-07-31",           "2026-06-01",           "2026-05-02"]
  }}
}`

func TestPreviousComparable(t *testing.T) {
	s, err := ParseSubmissions(strings.NewReader(historySubmissions))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		accession string
		items     []string
		want      string // accession number, "" for none
	}{
		{"earnings quarter over quarter", "0000320193-26-000300", []string{"2.02", "9.01"}, "0000320193-26-000200"},
		{"5.02 skips the 2.02 between", "0000320193-26-000250", []string{"5.02"}, "0000320193-26-000100"},
		{"9.01 is never the key", "0000320193-26-000250", []string{"9.01", "5.02"}, "0000320193-26-000100"},
		{"current not listed yet", "0000320193-26-000400", []string{"5.02", "9.01"}, "0000320193-26-000250"},
		{"only 9.01 uses it", "0000320193-26-000400", []string{"9.01"}, "0000320193-26-000300"},
		{"nothing older", "0000320193-26-000200", []string{"2.02", "9.01"}, ""},
		{"no such item", "0000320193-26-000300", []string{"1.01"}, ""},
		{"no items", "0000320193-26-000300", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := s.PreviousComparable(tt.accession, tt.items)
			if tt.want == "" {
				if ok {
					t.Fatalf("found %+v, want none", got)
				}
				return
			}
			if !ok || got.AccessionNo != tt.want {
				t.Fatalf("got %+v, %v; want %s", got, ok, tt.want)
			}
		})
	}

	got, _ := s.PreviousComparable("0000320193-26-000300", []string{"2.02", "9.01"})
	want := Comparable{AccessionNo: "0000320193-26-000200", FormType: "8-K/A", Items: []string{"2.02", "9.01"}, PrimaryDoc: "q3a.htm", FilingDate: "2026-07-31"}
	if got.AccessionNo != want.AccessionNo || got.FormType != want.FormType || !slices.Equal(got.Items, want.Items) ||
		got.PrimaryDoc != want.PrimaryDoc || got.FilingDate != want.FilingDate {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A filing date array that doesn't line up is dropped, not trusted.
func TestPreviousComparableWithoutDates(t *testing.T) {
	ragged := strings.Replace(historySubmissions, `"2026-10-30",           "2026-09-15",`, ``, 1)
	s, err := ParseSubmissions(strings.NewReader(ragged))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.PreviousComparable("0000320193-26-000300", []string{"2.02"})
	if !ok || got.AccessionNo != "0000320193-26-000200" || got.FilingDate != "" {
		t.Errorf("got %+v, %v", got, ok)
	}
}

func TestFilingFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != testAgent {
			t.Errorf("User-Agent = %q", got)
		}
		switch r.URL.Path {
		case "/Archives/edgar/data/320193/000032019326000300/index.json":
			_, _ = w.Write([]byte(`{"directory":{"name":"/Archives/edgar/data/320193/000032019326000300","item":[
				{"name":"q4.htm","type":"text.gif","size":"52311","last-modified":"2026-10-30 16:30:12"},
				{"name":"a8-kex991q4.htm","type":"text.gif","size":"120113","last-modified":"2026-10-30 16:30:12"},
				{"name":"0000320193-26-000300-index.html","type":"text.gif","size":"","last-modified":"2026-10-30 16:30:12"}
			]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, UserAgent: testAgent})
	ctx := context.Background()

	files, err := c.FilingFiles(ctx, "0000320193", "0000320193-26-000300")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files: %+v", len(files), files)
	}
	want := File{Name: "a8-kex991q4.htm", Type: "text.gif", URL: srv.URL + "/Archives/edgar/data/320193/000032019326000300/a8-kex991q4.htm"}
	if files[1] != want {
		t.Errorf("files[1] = %+v, want %+v", files[1], want)
	}
	if pr, ok := PressRelease(files); !ok || pr.Name != "a8-kex991q4.htm" {
		t.Errorf("press release = %+v, %v", pr, ok)
	}

	var se *StatusError
	if _, err := c.FilingFiles(ctx, "0000320193", "0000320193-26-999999"); !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Errorf("missing filing: err = %v, want a 404", err)
	}
}

func TestSubmissionsNeedsTenDigits(t *testing.T) {
	c, _ := NewClient(Config{DataURL: "http://127.0.0.1:1", UserAgent: testAgent})
	if _, err := c.Submissions(context.Background(), "320193"); err == nil {
		t.Error("a short CIK was accepted")
	}
}

func TestPressRelease(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string // "" for none
	}{
		{"ex99-1", []string{"q4.htm", "ex99-1.htm"}, "ex99-1.htm"},
		{"ex99_1", []string{"ex99_1.htm"}, "ex99_1.htm"},
		{"ex991 behind a prefix", []string{"aapl-20261030xex991.htm"}, "aapl-20261030xex991.htm"},
		{"ex-99.1", []string{"ex-99.1.htm"}, "ex-99.1.htm"},
		{"ex99d1", []string{"d123ex99d1.htm"}, "d123ex99d1.htm"},
		{"exhibit991", []string{"exhibit991.html"}, "exhibit991.html"},
		{"dex991 text", []string{"dex991.txt"}, "dex991.txt"},
		{"upper case", []string{"EX-99.1.HTM"}, "EX-99.1.HTM"},
		{"99.1 beats an earlier 99.2", []string{"ex99-2.htm", "ex99-1.htm"}, "ex99-1.htm"},
		{"99.10 isn't 99.1", []string{"ex99-10.htm", "ex99-1.htm"}, "ex99-1.htm"},
		{"another ex99 as fallback", []string{"q4.htm", "ex99-2.htm", "ex99-3.htm"}, "ex99-2.htm"},
		{"images and data skipped", []string{"ex991.jpg", "ex991.pdf", "ex991.xml", "ex991.xsd"}, ""},
		{"viewer and index pages skipped", []string{"R1.htm", "FilingSummary.xml", "FilingSummary.htm", "0000320193-26-000300-index.htm"}, ""},
		{"none", []string{"q4.htm", "ex10-1.htm"}, ""},
		{"empty", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var files []File
			for _, n := range tt.files {
				files = append(files, File{Name: n})
			}
			got, ok := PressRelease(files)
			if tt.want == "" {
				if ok {
					t.Fatalf("picked %q, want none", got.Name)
				}
				return
			}
			if !ok || got.Name != tt.want {
				t.Fatalf("got %q, %v; want %q", got.Name, ok, tt.want)
			}
		})
	}
}
