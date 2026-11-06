// Package edgar reads SEC EDGAR's "latest filings" feed.
//
// The feed lists a filing once per party: a Form 4 appears once for the
// company (Issuer) and once for each insider who traded (Reporting). ParseFeed
// returns those entries as they are; Group joins them into one Filing each.
package edgar

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/encoding/charmap"
)

// Entry is one line of the feed: one party to one filing.
type Entry struct {
	AccessionNo string    // the filing's unique ID, e.g. 0001193125-26-450499
	FormType    string    // 8-K, 10-Q, 4, SCHEDULE 13D/A…
	CIK         string    // the party's SEC ID, 10 digits
	Name        string    // the party's name
	Role        string    // Filer, Issuer, Subject, Reporting, Filed by…
	AcceptedAt  time.Time // when EDGAR accepted the filing
	URL         string    // the filing's index page on sec.gov
}

// Feed is one page of the feed.
type Feed struct {
	Entries []Entry
	Skipped int // entries without a usable accession number or time
}

// Party is one company or person named on a filing.
type Party struct {
	CIK  string `json:"cik"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// Filing is one filing with every party the feed listed for it. CIK and
// Company are the company the filing is about.
type Filing struct {
	AccessionNo string
	FormType    string
	CIK         string
	Company     string
	AcceptedAt  time.Time
	URL         string
	Parties     []Party
}

// The XML we read. Field names match Atom's; everything else is ignored.
type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	Title string `xml:"title"`
	Link  struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
	Updated  string `xml:"updated"`
	Category struct {
		Term string `xml:"term,attr"`
	} `xml:"category"`
	ID string `xml:"id"`
}

const idPrefix = "urn:tag:sec.gov,2008:accession-number="

var (
	accessionRE = regexp.MustCompile(`^\d{10}-\d{2}-\d{6}$`)
	// "<name> (<10-digit CIK>) (<role>)", read from the end because names can
	// contain parentheses themselves.
	partyRE = regexp.MustCompile(`^(.*) \((\d{10})\) \(([^()]+)\)$`)
)

// ParseFeed reads one page of the Atom feed. A page that isn't an Atom feed
// at all (an HTML error page, say) is an error; a single unreadable entry is
// skipped and counted, so one odd filing can't block the rest.
func ParseFeed(r io.Reader) (Feed, error) {
	dec := xml.NewDecoder(r)
	dec.CharsetReader = charsetReader // EDGAR declares ISO-8859-1, not UTF-8

	var raw atomFeed
	if err := dec.Decode(&raw); err != nil {
		return Feed{}, fmt.Errorf("parse feed: %w", err)
	}

	feed := Feed{Entries: make([]Entry, 0, len(raw.Entries))}
	for _, e := range raw.Entries {
		entry, ok := parseEntry(e)
		if !ok {
			feed.Skipped++
			continue
		}
		feed.Entries = append(feed.Entries, entry)
	}
	return feed, nil
}

func parseEntry(e atomEntry) (Entry, bool) {
	// The accession number is the key everything else hangs on. <id> is the
	// field meant for machines; the title and summary are meant for people.
	acc := strings.TrimPrefix(strings.TrimSpace(e.ID), idPrefix)
	if !accessionRE.MatchString(acc) {
		return Entry{}, false
	}
	// <updated> carries its UTC offset (-05:00 in winter, -04:00 in summer),
	// so no time zone database is needed.
	accepted, err := time.Parse(time.RFC3339, strings.TrimSpace(e.Updated))
	if err != nil {
		return Entry{}, false
	}

	out := Entry{
		AccessionNo: acc,
		FormType:    strings.TrimSpace(e.Category.Term),
		AcceptedAt:  accepted,
		URL:         strings.TrimSpace(e.Link.Href),
	}

	// The title reads "<form> - <name> (<CIK>) (<role>)". Names can contain
	// " - " too, so cut the known form off the front instead of splitting.
	title := strings.TrimSpace(e.Title)
	rest, found := strings.CutPrefix(title, out.FormType+" - ")
	if out.FormType == "" || !found {
		rest = title
		if form, after, ok := strings.Cut(title, " - "); ok {
			rest = after
			if out.FormType == "" {
				out.FormType = form
			}
		}
	}
	if m := partyRE.FindStringSubmatch(rest); m != nil {
		out.Name, out.CIK, out.Role = m[1], m[2], m[3]
	} else {
		out.Name = rest // keep the filing even if the title has an odd shape
	}
	return out, true
}

// subjectRank says how well a role names the company a filing is about.
// Lower is better. Roles not listed (Reporting, Filed by) name people or
// funds acting on a company, not the company itself.
var subjectRank = map[string]int{"Filer": 1, "Subject": 2, "Issuer": 3}

func rank(role string) int {
	if r, ok := subjectRank[role]; ok {
		return r
	}
	return len(subjectRank) + 1
}

// Group joins the entries of each filing into one Filing, keeping the feed's
// order (newest first). The company is the Filer, Subject or Issuer entry; if
// none was seen, the first party stands in.
func Group(entries []Entry) []Filing {
	var (
		out   []Filing
		roles []string // the role each filing's company came from
		index = map[string]int{}
	)
	for _, e := range entries {
		i, seen := index[e.AccessionNo]
		if !seen {
			i = len(out)
			index[e.AccessionNo] = i
			out = append(out, Filing{
				AccessionNo: e.AccessionNo,
				FormType:    e.FormType,
				CIK:         e.CIK,
				Company:     e.Name,
				AcceptedAt:  e.AcceptedAt,
				URL:         e.URL,
			})
			roles = append(roles, e.Role)
		}

		f := &out[i]
		p := Party{CIK: e.CIK, Name: e.Name, Role: e.Role}
		if !slices.Contains(f.Parties, p) {
			f.Parties = append(f.Parties, p)
		}
		if rank(e.Role) < rank(roles[i]) {
			f.CIK, f.Company, f.URL = e.CIK, e.Name, e.URL
			roles[i] = e.Role
		}
	}
	return out
}

func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "iso-8859-1", "latin1", "latin-1":
		return charmap.ISO8859_1.NewDecoder().Reader(input), nil
	case "windows-1252":
		return charmap.Windows1252.NewDecoder().Reader(input), nil
	}
	return nil, fmt.Errorf("unsupported charset %q", label)
}
