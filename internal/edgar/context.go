package edgar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Stage 3's context gathering: the filing's own documents, and the company's
// previous comparable 8-K to measure it against.

// File is one document inside a filing, from its index.json.
type File struct {
	Name string
	Type string // as the index reports it, may be empty
	URL  string
}

// Comparable is the company's previous filing to compare against.
type Comparable struct {
	AccessionNo string
	FormType    string
	Items       []string
	PrimaryDoc  string
	FilingDate  string // YYYY-MM-DD
}

// Submissions fetches a company's JSON (the same request Details makes). cik
// is the 10-digit form the feed gives, which is how SEC names the file.
func (c *Client) Submissions(ctx context.Context, cik string) (Submissions, error) {
	if len(cik) != 10 {
		return Submissions{}, fmt.Errorf("CIK %q is not 10 digits", cik)
	}
	body, err := c.get(ctx, c.cfg.DataURL+"/submissions/CIK"+cik+".json", "application/json")
	if err != nil {
		return Submissions{}, err
	}
	defer body.Close()
	return ParseSubmissions(io.LimitReader(body, maxBody))
}

// filingIndex is the part of a filing's index.json we read.
type filingIndex struct {
	Directory struct {
		Item []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"item"`
	} `json:"directory"`
}

// FilingFiles lists the documents in one filing.
func (c *Client) FilingFiles(ctx context.Context, cik, accessionNo string) ([]File, error) {
	body, err := c.get(ctx, DocumentURL(c.cfg.BaseURL, cik, accessionNo, "index.json"), "application/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var idx filingIndex
	if err := json.NewDecoder(io.LimitReader(body, maxBody)).Decode(&idx); err != nil {
		return nil, fmt.Errorf("parse filing index: %w", err)
	}
	files := make([]File, 0, len(idx.Directory.Item))
	for _, it := range idx.Directory.Item {
		if it.Name == "" {
			continue
		}
		files = append(files, File{
			Name: it.Name,
			Type: it.Type,
			URL:  DocumentURL(c.cfg.BaseURL, cik, accessionNo, it.Name),
		})
	}
	return files, nil
}

// Filers name exhibits every which way: ex99-1, ex99_1, ex991, ex-99.1,
// ex99d1, exhibit991, dex991, often behind a ticker prefix. The trailing
// non-digit keeps 99.10 and up from passing for 99.1.
var (
	ex991 = regexp.MustCompile(`ex(?:hibit)?[-_.]?99(?:[-_.]|d)?0?1(?:[^0-9]|$)`)
	ex99  = regexp.MustCompile(`ex(?:hibit)?[-_.]?99`)
	rPage = regexp.MustCompile(`^r[0-9]+\.htm$`) // XBRL viewer pages
)

// PressRelease picks the Exhibit 99.1-style press release from a filing's
// files, if there is one. Some filers number the release 99.2 or leave the
// number off, so any other Exhibit 99 document is the fallback.
func PressRelease(files []File) (File, bool) {
	fallback, haveFallback := File{}, false
	for _, f := range files {
		name := strings.ToLower(path.Base(f.Name))
		switch path.Ext(name) {
		case ".htm", ".html", ".txt":
		default:
			continue
		}
		if rPage.MatchString(name) || strings.HasPrefix(name, "filingsummary") || strings.Contains(name, "index") {
			continue
		}
		if ex991.MatchString(name) {
			return f, true
		}
		if !haveFallback && ex99.MatchString(name) {
			fallback, haveFallback = f, true
		}
	}
	return fallback, haveFallback
}

// PreviousComparable finds the newest earlier 8-K that shares the given
// filing's most important item. ok is false when there's none.
//
// 9.01 (exhibits) rides along with almost every 8-K, so it says nothing about
// what the filing is; the first other item does.
func (s Submissions) PreviousComparable(accessionNo string, items []string) (Comparable, bool) {
	if len(items) == 0 {
		return Comparable{}, false
	}
	key := items[0]
	for _, it := range items {
		if it != "9.01" {
			key = it
			break
		}
	}
	rec := s.Filings.Recent
	// SEC lists filings newest first. When the current one isn't listed yet
	// (the JSON lags the feed a little), everything listed is older.
	start := 0
	if i := slices.Index(rec.AccessionNumber, accessionNo); i >= 0 {
		start = i + 1
	}
	for i := start; i < len(rec.AccessionNumber); i++ {
		if rec.AccessionNumber[i] == accessionNo {
			continue
		}
		if form := rec.Form[i]; form != "8-K" && form != "8-K/A" {
			continue
		}
		its := splitItems(rec.Items[i])
		if !slices.Contains(its, key) {
			continue
		}
		c := Comparable{
			AccessionNo: rec.AccessionNumber[i],
			FormType:    rec.Form[i],
			Items:       its,
			PrimaryDoc:  rec.PrimaryDocument[i],
		}
		if rec.FilingDate != nil {
			c.FilingDate = rec.FilingDate[i]
		}
		return c, true
	}
	return Comparable{}, false
}
