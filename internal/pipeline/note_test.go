package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
	"github.com/Yerassyl20036/filing-radar/internal/model"
	"github.com/Yerassyl20036/filing-radar/internal/store"
)

const (
	prevAcc     = "0000320193-26-000050"
	currentText = `Item 2.02 Results of Operations. On November 5, 2026, the Company reported revenue of $102.5 billion for the fourth quarter.`
	releaseText = `The board declared a cash dividend of $0.27 per share, up from $0.26.`
	prevText    = `Item 2.02 Results of Operations. On August 6, 2026, the Company reported revenue of $94.0 billion for the third quarter.`
)

// fakeArchive serves documents by accession and name; anything else is a 404.
type fakeArchive struct {
	subs  edgar.Submissions
	files map[string][]edgar.File
	docs  map[string]string // accession + "/" + name
}

func (f *fakeArchive) Document(_ context.Context, _, accessionNo, name string) (string, error) {
	if t, ok := f.docs[accessionNo+"/"+name]; ok {
		return t, nil
	}
	return "", &edgar.StatusError{Code: 404}
}

func (f *fakeArchive) Submissions(context.Context, string) (edgar.Submissions, error) {
	return f.subs, nil
}

func (f *fakeArchive) FilingFiles(_ context.Context, _, accessionNo string) ([]edgar.File, error) {
	if fs, ok := f.files[accessionNo]; ok {
		return fs, nil
	}
	return nil, &edgar.StatusError{Code: 404}
}

// newArchive has the current 8-K with a press release, and a previous one
// with the same lead item unless withPrevious is false.
func newArchive(withPrevious bool) *fakeArchive {
	f := &fakeArchive{
		files: map[string][]edgar.File{acc: {{Name: "aapl.htm"}, {Name: "aapl-ex991.htm"}}},
		docs:  map[string]string{acc + "/aapl.htm": currentText, acc + "/aapl-ex991.htm": releaseText},
	}
	rec := &f.subs.Filings.Recent
	rec.AccessionNumber = []string{acc, "0000320193-26-000070", prevAcc}
	rec.Form = []string{"8-K", "4", "8-K"}
	rec.Items = []string{"2.02,9.01", "", "2.02,9.01"}
	rec.PrimaryDocument = []string{"aapl.htm", "form4.xml", "q3.htm"}
	rec.FilingDate = []string{"2026-11-05", "2026-09-01", "2026-08-06"}
	if withPrevious {
		f.docs[prevAcc+"/q3.htm"] = prevText
	} else {
		rec.Items[2] = "5.02"
	}
	return f
}

// fakeWriter answers Draft and then Repair with drafts, in order. Each call
// is billed unless unbilled is set and the call fails.
type fakeWriter struct {
	drafts   []model.Draft
	err      error
	unbilled bool
	calls    int
	inputs   []model.NoteInput
	failed   []int // what Repair was asked to fix
}

func (w *fakeWriter) next(in model.NoteInput) (model.Draft, error) {
	w.calls++
	w.inputs = append(w.inputs, in)
	d := model.Draft{}
	if len(w.drafts) > 0 {
		d = w.drafts[min(w.calls, len(w.drafts))-1]
	}
	d.Model, d.Usage = "deepseek-flash", model.Usage{InputTokens: 9000, OutputTokens: 400, CostUSD: 0.0032}
	if w.err != nil && w.unbilled {
		d.Usage = model.Usage{}
	}
	return d, w.err
}

// estimatingWriter prices every call at est before it is made.
type estimatingWriter struct {
	*fakeWriter
	est float64
}

func (w estimatingWriter) Estimate(int) model.Usage {
	return model.Usage{InputTokens: 40_000, OutputTokens: 4096, CostUSD: w.est}
}

func (w *fakeWriter) Draft(_ context.Context, in model.NoteInput) (model.Draft, error) {
	return w.next(in)
}

func (w *fakeWriter) Repair(_ context.Context, in model.NoteInput, _ model.Draft, failed []int) (model.Draft, error) {
	w.failed = failed
	return w.next(in)
}

var (
	goodRevenue  = model.DraftClaim{Text: "Revenue was $102.5 billion.", Quote: "reported revenue of $102.5 billion for the fourth quarter", Source: "current"}
	goodDividend = model.DraftClaim{Text: "The dividend rose a cent.", Quote: "cash dividend of $0.27 per share, up from $0.26", Source: "current"}
	goodPrevious = model.DraftClaim{Text: "Last quarter's revenue was $94.0 billion.", Quote: "the Company reported revenue of $94.0 billion", Source: "previous"}
	badQuote     = model.DraftClaim{Text: "Revenue doubled.", Quote: "revenue doubled year over year", Source: "current"}
)

var task = store.NoteTask{
	AccessionNo: acc, FormType: "8-K", CIK: "0000320193", Company: "Apple Inc.",
	Items: []string{"2.02", "9.01"}, PrimaryDoc: "aapl.htm",
	AcceptedAt: time.Date(2026, 11, 5, 21, 30, 0, 0, time.UTC), // 4:30 pm in New York
}

func runNote(t *testing.T, st *fakeStore, ar *fakeArchive, w *fakeWriter) NoteResult {
	t.Helper()
	return runNoteActs(t, &Activities{Archive: ar, Store: st, Writer: w, Provider: "deepseek", Budget: 0.20})
}

func runNoteActs(t *testing.T, acts *Activities) NoteResult {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(acts)
	env.ExecuteWorkflow(NoteWorkflow, task)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow didn't complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res NoteResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	return res
}

func purposes(st *fakeStore) []string {
	var out []string
	for _, c := range st.calls {
		out = append(out, c.Purpose)
	}
	return out
}

func TestNotePublished(t *testing.T) {
	st := newFakeStore()
	w := &fakeWriter{drafts: []model.Draft{{Headline: "Revenue up on last quarter", Claims: []model.DraftClaim{goodRevenue, goodPrevious}}}}
	res := runNote(t, st, newArchive(true), w)

	if res.Status != NotePublished || res.Claims != 2 || w.calls != 1 {
		t.Fatalf("got %+v after %d calls", res, w.calls)
	}
	n := st.notes[acc]
	if n.Status != "published" || n.Headline != "Revenue up on last quarter" || n.PreviousAccessionNo != prevAcc ||
		n.Model != "deepseek-flash" || n.PromptVersion != model.PromptVersion {
		t.Errorf("saved %+v", n)
	}
	wantURLs := []string{
		"https://www.sec.gov/Archives/edgar/data/320193/000032019326000101/aapl.htm",
		"https://www.sec.gov/Archives/edgar/data/320193/000032019326000050/q3.htm",
	}
	if len(n.Claims) != 2 || n.Claims[0].URL != wantURLs[0] || n.Claims[1].URL != wantURLs[1] {
		t.Errorf("claims %+v", n.Claims)
	}
	in := w.inputs[0]
	if in.Current.Label != "this filing (8-K, 2026-11-05)" || in.Previous == nil || in.Previous.Label != "previous filing (8-K, 2026-08-06)" {
		t.Errorf("labels %q, %+v", in.Current.Label, in.Previous)
	}
	if want := []string{"draft"}; !slices.Equal(purposes(st), want) || st.calls[0].Provider != "deepseek" {
		t.Errorf("recorded %+v", st.calls)
	}
}

func TestNoteRepairFixesAClaim(t *testing.T) {
	st := newFakeStore()
	w := &fakeWriter{drafts: []model.Draft{
		{Headline: "h", Claims: []model.DraftClaim{goodRevenue, badQuote, goodDividend}},
		{Headline: "h", Claims: []model.DraftClaim{goodRevenue, goodPrevious, goodDividend}},
	}}
	res := runNote(t, st, newArchive(true), w)

	if res.Status != NotePublished || res.Claims != 3 || w.calls != 2 || !slices.Equal(w.failed, []int{1}) {
		t.Fatalf("got %+v after %d calls, repair of %v", res, w.calls, w.failed)
	}
	if want := []string{"draft", "repair"}; !slices.Equal(purposes(st), want) {
		t.Errorf("recorded %v", purposes(st))
	}
}

// A claim the repair can't fix is dropped; one claim left is too few.
func TestNoteRejectedWhenRepairFails(t *testing.T) {
	st := newFakeStore()
	bad := model.Draft{Headline: "h", Claims: []model.DraftClaim{goodRevenue, badQuote, {Text: "x", Quote: "y", Source: "previous"}}}
	w := &fakeWriter{drafts: []model.Draft{bad, bad}}
	res := runNote(t, st, newArchive(true), w)

	if res.Status != NoteRejected || w.calls != 2 {
		t.Fatalf("got %+v after %d calls", res, w.calls)
	}
	n := st.notes[acc]
	if n.Status != "rejected" || n.Reason != "only 1 checked claims" || n.PreviousAccessionNo != prevAcc || n.PromptVersion != model.PromptVersion {
		t.Errorf("saved %+v", n)
	}
}

// A spent budget leaves no note, so the filing is tried again tomorrow.
func TestNoteWaitsForTheBudget(t *testing.T) {
	st := newFakeStore()
	st.spent = 0.25
	w := &fakeWriter{}
	res := runNote(t, st, newArchive(true), w)

	if res.Status != NoteSkipped || w.calls != 0 || len(st.calls) != 0 {
		t.Fatalf("got %+v after %d calls", res, w.calls)
	}
	if _, ok := st.notes[acc]; ok {
		t.Error("a note was saved")
	}
}

// No earlier filing with the same lead item: the note covers this one alone,
// press release included.
func TestNoteWithoutPrevious(t *testing.T) {
	st := newFakeStore()
	w := &fakeWriter{drafts: []model.Draft{{Headline: "h", Claims: []model.DraftClaim{goodRevenue, goodDividend}}}}
	res := runNote(t, st, newArchive(false), w)

	if res.Status != NotePublished || w.inputs[0].Previous != nil {
		t.Fatalf("got %+v, previous %+v", res, w.inputs[0].Previous)
	}
	if n := st.notes[acc]; n.PreviousAccessionNo != "" || len(n.Claims) != 2 {
		t.Errorf("saved %+v", n)
	}
}

// How a failed draft ends depends on whether it was paid for. A paid or
// refused one is rejected after one call; one the provider never answered
// leaves no row, so a later run tries again. No path pays for a draft twice.
func TestNoteDraftFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		unbilled bool
		est      float64 // the writer's estimate; 0 means it gives none
		status   string
		calls    int
		reason   string
		recorded []store.ModelCall
		held     bool
	}{
		{name: "paid, unusable", err: errors.New("the model ran out of room before answering"),
			status: NoteRejected, calls: 1, reason: "the model couldn't write a note: the model ran out of room before answering",
			recorded: []store.ModelCall{{AccessionNo: acc, Purpose: "draft", Provider: "deepseek", Model: "deepseek-flash", InputTokens: 9000, OutputTokens: 400, CostUSD: 0.0032}}},
		{name: "request refused", err: &model.PermanentError{Err: errors.New("deepseek rejected the request (422)")}, unbilled: true,
			status: NoteRejected, calls: 1, reason: "the model couldn't write a note: deepseek rejected the request (422)"},
		{name: "no balance", err: &model.PermanentError{Err: &model.AccountError{Err: errors.New("deepseek rejected the request (402)")}}, unbilled: true,
			status: NoteSkipped, calls: 1, held: true},
		{name: "outage", err: errors.New("deepseek request failed (503)"), unbilled: true,
			status: NoteSkipped, calls: 3},
		// A call cut off by a timeout may have been billed: counted at its
		// estimate, and not tried again.
		{name: "timed out", err: fmt.Errorf("deepseek request: %w", context.DeadlineExceeded), unbilled: true, est: 0.02,
			status: NoteRejected, calls: 1, reason: "the model couldn't write a note: deepseek request: context deadline exceeded",
			recorded: []store.ModelCall{{AccessionNo: acc, Purpose: "draft", Provider: "deepseek", Model: "(cut off; cost estimated)", InputTokens: 40_000, OutputTokens: 4096, CostUSD: 0.02}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			w := &fakeWriter{err: tc.err, unbilled: tc.unbilled}
			acts := &Activities{Archive: newArchive(true), Store: st, Writer: w, Provider: "deepseek", Budget: 0.20, Hold: &Hold{}}
			if tc.est > 0 {
				acts.Writer = estimatingWriter{w, tc.est}
			}
			res := runNoteActs(t, acts)

			if res.Status != tc.status || w.calls != tc.calls {
				t.Fatalf("got %+v after %d calls", res, w.calls)
			}
			n, saved := st.notes[acc]
			if saved != (tc.status == NoteRejected) || (saved && n.Reason != tc.reason) {
				t.Errorf("saved %v: %+v", saved, n)
			}
			if !slices.Equal(st.calls, tc.recorded) {
				t.Errorf("recorded %+v", st.calls)
			}
			if acts.Hold.Active() != tc.held {
				t.Errorf("held = %v", acts.Hold.Active())
			}
		})
	}
}

// Dropped claims take the headline with them: it may say what they said.
func TestNoteHeadlineNotKeptAfterDroppedClaims(t *testing.T) {
	for _, tc := range []struct {
		name  string
		draft model.Draft
		want  string
	}{
		{"claim dropped", model.Draft{Headline: "Revenue doubled, CFO resigns", Claims: []model.DraftClaim{goodRevenue, badQuote, goodDividend}}, goodRevenue.Text},
		{"number not in any quote", model.Draft{Headline: "Revenue hits $200 billion", Claims: []model.DraftClaim{goodRevenue, goodDividend}}, goodRevenue.Text},
		{"backed", model.Draft{Headline: "Revenue reaches $102.5 billion", Claims: []model.DraftClaim{goodRevenue, goodDividend}}, "Revenue reaches $102.5 billion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			w := &fakeWriter{drafts: []model.Draft{tc.draft}} // the repair returns the same draft
			res := runNote(t, st, newArchive(true), w)
			if res.Status != NotePublished || st.notes[acc].Headline != tc.want {
				t.Errorf("got %+v, headline %q", res, st.notes[acc].Headline)
			}
		})
	}
}

// Calls running at once each hold their estimate, so they can't all pass
// the same budget check.
func TestReserveCountsCallsInFlight(t *testing.T) {
	st := newFakeStore()
	st.spent = 0.08
	a := &Activities{Store: st, Budget: 0.20}
	ctx := context.Background()
	var releases []func()
	for i, want := range []bool{true, true, false} { // 0.08 + 0.05 + 0.05, then 0.23 is over
		release, ok, err := a.reserve(ctx, 0.05)
		if err != nil || ok != want {
			t.Fatalf("reservation %d: ok %v, err %v", i, ok, err)
		}
		if ok {
			releases = append(releases, release)
		}
	}
	if over, _ := a.overBudget(ctx); over {
		t.Error("over budget at 0.18 of 0.20")
	}
	releases[0]()
	if _, ok, _ := a.reserve(ctx, 0.05); !ok {
		t.Error("a released reservation still counts")
	}
}

func TestNoteMissingMainDocument(t *testing.T) {
	st := newFakeStore()
	ar := newArchive(true)
	delete(ar.docs, acc+"/aapl.htm")
	w := &fakeWriter{}
	res := runNote(t, st, ar, w)
	if res.Status != NoteRejected || w.calls != 0 || st.notes[acc].Reason != "the main document wasn't found" {
		t.Errorf("got %+v, saved %+v", res, st.notes[acc])
	}
}

func TestDispatchNotes(t *testing.T) {
	tasks := []store.NoteTask{{AccessionNo: "a"}, {AccessionNo: "b"}, {AccessionNo: "c"}, {AccessionNo: "d"}}
	for _, tc := range []struct {
		name       string
		notesToday int
		spent      float64
		cap        int
		want       DispatchResult
	}{
		{"within the cap", 1, 0, 3, DispatchResult{NotesStarted: 2}},
		{"cap reached", 3, 0, 3, DispatchResult{}},
		{"budget spent", 0, 0.20, 3, DispatchResult{}},
		{"notes off", 0, 0, 0, DispatchResult{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			st.toWrite, st.notesToday, st.spent = tasks, tc.notesToday, tc.spent
			tc2 := &fakeTemporal{started: map[string]bool{}}
			d := &Dispatcher{Store: st, Temporal: tc2, Batch: 100, NoteCap: tc.cap, Budget: 0.20}
			res, err := d.Dispatch(context.Background())
			if err != nil || res != tc.want {
				t.Fatalf("got %+v, %v", res, err)
			}
			if tc.want.NotesStarted > 0 && !tc2.started[NoteWorkflowID("a")] {
				t.Errorf("started %v", tc2.started)
			}
			// The next run finds the same filings still without a note.
			res, err = d.Dispatch(context.Background())
			if err != nil || res.NotesStarted != 0 || res.NotesRunning != tc.want.NotesStarted {
				t.Errorf("second run: %+v, %v", res, err)
			}
		})
	}
}

// Running note workflows have no row yet, but count against the cap: a new
// filing doesn't start a fourth while three run.
func TestDispatchNotesCountsRunning(t *testing.T) {
	st := newFakeStore()
	st.toWrite = []store.NoteTask{{AccessionNo: "a"}, {AccessionNo: "b"}, {AccessionNo: "c"}, {AccessionNo: "d"}}
	tc := &fakeTemporal{started: map[string]bool{}}
	hold := &Hold{}
	d := &Dispatcher{Store: st, Temporal: tc, Batch: 100, NoteCap: 3, Budget: 0.20, Hold: hold}
	if res, err := d.Dispatch(context.Background()); err != nil || res.NotesStarted != 3 {
		t.Fatalf("first run: %+v, %v", res, err)
	}

	st.toWrite = append([]store.NoteTask{{AccessionNo: "e"}}, st.toWrite...) // newest first
	if res, err := d.Dispatch(context.Background()); err != nil || res.NotesStarted != 0 || res.NotesRunning != 3 {
		t.Fatalf("with a new filing: %+v, %v", res, err)
	}

	// a's note lands: it now counts as done, and still nothing is free.
	st.toWrite = slices.DeleteFunc(st.toWrite, func(t store.NoteTask) bool { return t.AccessionNo == "a" })
	st.notesToday = 1
	if res, err := d.Dispatch(context.Background()); err != nil || res.NotesStarted != 0 || tc.started[NoteWorkflowID("e")] {
		t.Fatalf("after a's note: %+v, %v", res, err)
	}

	// While model calls are held, nothing starts at all.
	hold.start(time.Minute)
	st.notesToday = 0
	if res, err := d.Dispatch(context.Background()); err != nil || res.NotesStarted+res.NotesRunning != 0 {
		t.Fatalf("held: %+v, %v", res, err)
	}
}
