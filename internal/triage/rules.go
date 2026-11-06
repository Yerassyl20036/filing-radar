// Package triage decides how much a filing matters, with plain rules a person
// can check. Rules are free, instant and explainable, so they go first; only
// what they flag will ever reach a model (stage 2b).
package triage

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// RulesVersion changes whenever the rules do. Each label records the version
// that produced it, so old labels can be redone from stored facts.
const RulesVersion = 1

type Level string

const (
	High    Level = "high"
	Medium  Level = "medium"
	Low     Level = "low"
	Routine Level = "routine"
)

var order = map[Level]int{Routine: 0, Low: 1, Medium: 2, High: 3}

// AtLeast says whether l is l2 or more important.
func (l Level) AtLeast(l2 Level) bool { return order[l] >= order[l2] }

// ParseLevel accepts the four level names.
func ParseLevel(s string) (Level, bool) {
	l := Level(s)
	_, ok := order[l]
	return l, ok
}

// Result is a label and why.
type Result struct {
	Level   Level    `json:"level"`
	Reasons []string `json:"reasons"`
}

type rule struct {
	level Level
	what  string
}

// items are the 8-K item numbers, with what each one means. A filing takes
// the level of its most important item.
var items = map[string]rule{
	"1.01": {High, "material agreement signed"},
	"1.02": {Medium, "material agreement ended"},
	"1.03": {High, "bankruptcy or receivership"},
	"1.04": {Medium, "mine safety violation"},
	"1.05": {High, "material cybersecurity incident"},
	"2.01": {High, "acquisition or sale completed"},
	"2.02": {High, "earnings results"},
	"2.03": {Medium, "new debt"},
	"2.04": {High, "debt payments accelerated"},
	"2.05": {Medium, "restructuring or layoffs"},
	"2.06": {High, "material impairment"},
	"3.01": {High, "delisting notice"},
	"3.02": {Medium, "shares sold privately"},
	"3.03": {Medium, "shareholder rights changed"},
	"4.01": {Medium, "auditor changed"},
	"4.02": {High, "past financial statements can't be relied on"},
	"5.01": {High, "change in control"},
	"5.02": {Medium, "director or officer change"},
	"5.03": {Low, "bylaws or fiscal year changed"},
	"5.04": {Low, "retirement plan trading suspended"},
	"5.05": {Low, "code of ethics changed"},
	"5.06": {Medium, "no longer a shell company"},
	"5.07": {Low, "shareholder vote results"},
	"5.08": {Low, "shareholder nominations"},
	"7.01": {Low, "Regulation FD disclosure"},
	"8.01": {Low, "other events"},
	"9.01": {Low, "financial statements and exhibits"},
}

// forms are labelled by type alone. "Track" means the filing gets a workflow
// (details fetched, later read by agents); the rest are labelled on the spot.
var forms = map[string]struct {
	rule
	track bool
}{
	"8-K":            {rule{Low, "current report"}, true}, // the items decide
	"10-K":           {rule{Medium, "annual report"}, true},
	"10-Q":           {rule{Medium, "quarterly report"}, true},
	"20-F":           {rule{Medium, "foreign annual report"}, true},
	"40-F":           {rule{Medium, "Canadian annual report"}, true},
	"6-K":            {rule{Low, "foreign current report"}, true},
	"S-1":            {rule{Medium, "IPO registration"}, true},
	"F-1":            {rule{Medium, "foreign IPO registration"}, true},
	"SCHEDULE 13D":   {rule{Medium, "activist stake over 5%"}, true},
	"SC 13D":         {rule{Medium, "activist stake over 5%"}, true},
	"SCHEDULE 13G":   {rule{Routine, "passive stake over 5%"}, false},
	"SC 13G":         {rule{Routine, "passive stake over 5%"}, false},
	"3":              {rule{Routine, "insider's first ownership report"}, false},
	"4":              {rule{Routine, "insider trade"}, false},
	"5":              {rule{Routine, "insider's annual report"}, false},
	"144":            {rule{Routine, "planned insider sale"}, false},
	"SCHEDULE 13D/A": {rule{Medium, "activist stake changed"}, true},
	"SC 13D/A":       {rule{Medium, "activist stake changed"}, true},
}

// base splits "10-K/A" into "10-K" and true. Schedule 13D amendments have
// their own entry above, because a changed stake is news in itself.
func base(form string) (string, bool) {
	form = strings.TrimSpace(form)
	if _, ok := forms[form]; ok {
		return form, false
	}
	if b, ok := strings.CutSuffix(form, "/A"); ok {
		return b, true
	}
	return form, false
}

// Track says whether a filing of this form gets a workflow. Insider forms,
// fund paperwork and the rest are labelled at once and never cost a request.
func Track(form string) bool {
	b, _ := base(form)
	f, ok := forms[b]
	return ok && f.track
}

// Classify labels a filing from its form and, for 8-Ks, its item numbers.
// detailsFound is false when SEC's details couldn't be read; the label then
// rests on the form alone and says so.
func Classify(form string, itemNos []string, detailsFound bool) Result {
	b, amended := base(form)
	f, known := forms[b]
	if !known {
		return Result{Level: Routine, Reasons: []string{"form " + form + " is not tracked"}}
	}

	res := Result{Level: f.level, Reasons: []string{f.what}}
	if b == "8-K" {
		res = fromItems(itemNos)
		if !detailsFound {
			res.Reasons = append(res.Reasons, "item numbers unavailable")
		}
	}
	if amended {
		res.Reasons = append(res.Reasons, "an amendment")
	}
	return res
}

func fromItems(itemNos []string) Result {
	type hit struct {
		no string
		rule
	}
	var hits []hit
	for _, no := range itemNos {
		no = strings.TrimSpace(no)
		if r, ok := items[no]; ok {
			hits = append(hits, hit{no, r})
		} else if no != "" {
			hits = append(hits, hit{no, rule{Low, "unlisted item"}})
		}
	}
	if len(hits) == 0 {
		return Result{Level: Low, Reasons: []string{"no item numbers"}}
	}
	// Most important first, then by item number, so reasons read well.
	slices.SortStableFunc(hits, func(a, b hit) int {
		return cmp.Or(order[b.level]-order[a.level], strings.Compare(a.no, b.no))
	})
	res := Result{Level: hits[0].level}
	for _, h := range hits {
		res.Reasons = append(res.Reasons, "Item "+h.no+": "+h.what)
	}
	return res
}

// ScoreLevel turns a model's 0–100 score into a level.
func ScoreLevel(score int) Level {
	switch {
	case score >= 70:
		return High
	case score >= 40:
		return Medium
	case score >= 15:
		return Low
	}
	return Routine
}

// Raise applies a model's score to a rules label. The model can raise a
// label, never lower it: a restatement stays high even if the model shrugs.
// Its job is to catch what rules can't read, like big news filed under 8.01.
func Raise(r Result, score int) Result {
	l := ScoreLevel(score)
	if !l.AtLeast(r.Level) || l == r.Level {
		return r
	}
	out := Result{Level: l, Reasons: append(slices.Clone(r.Reasons), fmt.Sprintf("raised by the model (score %d)", score))}
	return out
}
