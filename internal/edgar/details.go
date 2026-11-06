package edgar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Details is what SEC's per-company JSON says about one filing.
type Details struct {
	Items      []string // 8-K item numbers, e.g. ["2.02", "9.01"]; empty for other forms
	PrimaryDoc string   // the main document's file name
}

// ErrNotListed means the company's JSON doesn't list the filing yet. It's
// usually only seconds behind the feed, so the caller should try again later.
var ErrNotListed = errors.New("filing not in the company's list yet")

// Submissions is the part of a company's JSON we read: its recent filings,
// as parallel arrays where position i of every array describes filing i.
type Submissions struct {
	CIK     string `json:"cik"`
	Name    string `json:"name"`
	Filings struct {
		Recent struct {
			AccessionNumber []string `json:"accessionNumber"`
			Form            []string `json:"form"`
			Items           []string `json:"items"`
			PrimaryDocument []string `json:"primaryDocument"`
		} `json:"recent"`
	} `json:"filings"`
}

// ParseSubmissions reads a company's JSON. Unused fields are skipped.
func ParseSubmissions(r io.Reader) (Submissions, error) {
	var s Submissions
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return Submissions{}, fmt.Errorf("parse company filings: %w", err)
	}
	rec := s.Filings.Recent
	if n := len(rec.AccessionNumber); len(rec.Form) != n || len(rec.Items) != n || len(rec.PrimaryDocument) != n {
		return Submissions{}, fmt.Errorf("parse company filings: arrays of different lengths (%d accession numbers, %d forms, %d items, %d documents)",
			n, len(rec.Form), len(rec.Items), len(rec.PrimaryDocument))
	}
	return s, nil
}

// Find returns one filing's details.
func (s Submissions) Find(accessionNo string) (Details, bool) {
	rec := s.Filings.Recent
	for i, a := range rec.AccessionNumber {
		if a != accessionNo {
			continue
		}
		d := Details{PrimaryDoc: rec.PrimaryDocument[i]}
		for _, it := range strings.Split(rec.Items[i], ",") {
			if it = strings.TrimSpace(it); it != "" {
				d.Items = append(d.Items, it)
			}
		}
		return d, true
	}
	return Details{}, false
}

// Details fetches the company's JSON and finds one filing in it. cik is the
// 10-digit form the feed gives.
func (c *Client) Details(ctx context.Context, cik, accessionNo string) (Details, error) {
	if len(cik) != 10 {
		return Details{}, fmt.Errorf("CIK %q is not 10 digits", cik)
	}
	body, err := c.get(ctx, c.cfg.DataURL+"/submissions/CIK"+cik+".json", "application/json")
	if err != nil {
		return Details{}, err
	}
	defer body.Close()
	s, err := ParseSubmissions(io.LimitReader(body, maxBody))
	if err != nil {
		return Details{}, err
	}
	d, ok := s.Find(accessionNo)
	if !ok {
		return Details{}, ErrNotListed
	}
	return d, nil
}

// DocumentURL is where a filing's main document lives on sec.gov.
func DocumentURL(baseURL, cik, accessionNo, primaryDoc string) string {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return fmt.Sprintf("%s/Archives/edgar/data/%s/%s/%s",
		strings.TrimRight(baseURL, "/"), strings.TrimLeft(cik, "0"), strings.ReplaceAll(accessionNo, "-", ""), primaryDoc)
}
