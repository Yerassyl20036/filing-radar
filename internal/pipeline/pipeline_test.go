package pipeline

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
	"github.com/Yerassyl20036/filing-radar/internal/model"
	"github.com/Yerassyl20036/filing-radar/internal/store"
	"github.com/Yerassyl20036/filing-radar/internal/triage"
)

type fakeStore struct {
	mu         sync.Mutex
	details    map[string]edgar.Details
	labels     map[string]store.Triage
	undecided  []store.Undecided
	modelCalls int
	spent      float64 // spend before any recorded call
	calls      []store.ModelCall
	notes      map[string]store.Note
	toWrite    []store.NoteTask
	notesToday int
	outcomes   map[string]store.Outcome
	toTrack    []store.OutcomeTask
}

func newFakeStore() *fakeStore {
	return &fakeStore{details: map[string]edgar.Details{}, labels: map[string]store.Triage{}, notes: map[string]store.Note{}, outcomes: map[string]store.Outcome{}}
}

func (s *fakeStore) RecordModelCall(_ context.Context, c store.ModelCall) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	return nil
}

func (s *fakeStore) ModelSpendToday(context.Context) (float64, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	usd := s.spent
	for _, c := range s.calls {
		usd += c.CostUSD
	}
	return usd, len(s.calls), nil
}

func (s *fakeStore) SaveNote(_ context.Context, n store.Note) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes[n.AccessionNo] = n
	return nil
}

func (s *fakeStore) NotesToWrite(_ context.Context, limit int) ([]store.NoteTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toWrite[:min(limit, len(s.toWrite))], nil
}

func (s *fakeStore) NotesToday(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notesToday, nil
}

func (s *fakeStore) Details(_ context.Context, acc string) ([]string, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.details[acc]
	return d.Items, d.PrimaryDoc, ok, nil
}

func (s *fakeStore) SaveDetails(_ context.Context, acc string, items []string, doc string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.details[acc] = edgar.Details{Items: items, PrimaryDoc: doc}
	return nil
}

func (s *fakeStore) SaveTriage(_ context.Context, t store.Triage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labels[t.AccessionNo] = t
	return nil
}

func (s *fakeStore) Undecided(context.Context, int, int) ([]store.Undecided, error) {
	return s.undecided, nil
}

func (s *fakeStore) ModelCallsToday(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelCalls, nil
}

// fakeSEC answers with each error in turn, then with d.
type fakeSEC struct {
	errs  []error
	d     edgar.Details
	calls int
}

func (f *fakeSEC) Details(context.Context, string, string) (edgar.Details, error) {
	f.calls++
	if f.calls <= len(f.errs) {
		return edgar.Details{}, f.errs[f.calls-1]
	}
	return f.d, nil
}

func runWorkflow(t *testing.T, acts *Activities, f Filing) triage.Result {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(acts)
	env.ExecuteWorkflow(FilingWorkflow, f)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow didn't complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res triage.Result
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	return res
}

const acc = "0000320193-26-000101"

var earnings = Filing{AccessionNo: acc, FormType: "8-K", CIK: "0000320193"}

func TestWorkflowLabelsAnEightK(t *testing.T) {
	st := newFakeStore()
	sec := &fakeSEC{d: edgar.Details{Items: []string{"2.02", "9.01"}, PrimaryDoc: "aapl.htm"}}
	res := runWorkflow(t, &Activities{SEC: sec, Store: st}, earnings)

	if res.Level != triage.High {
		t.Errorf("level %s, want high", res.Level)
	}
	if got := st.labels[acc]; got.Level != "high" || got.RulesVersion != triage.RulesVersion {
		t.Errorf("saved %+v", got)
	}
	if !slices.Equal(st.details[acc].Items, []string{"2.02", "9.01"}) {
		t.Errorf("details not stored: %+v", st.details[acc])
	}
}

// The company's list lags the feed by a moment; the fetch is retried.
func TestWorkflowWaitsForTheCompanyList(t *testing.T) {
	st := newFakeStore()
	sec := &fakeSEC{errs: []error{edgar.ErrNotListed, edgar.ErrNotListed}, d: edgar.Details{Items: []string{"5.02"}}}
	res := runWorkflow(t, &Activities{SEC: sec, Store: st}, earnings)
	if res.Level != triage.Medium || sec.calls != 3 {
		t.Errorf("level %s after %d calls, want medium after 3", res.Level, sec.calls)
	}
}

// No JSON for the company at all: don't retry, label from the form.
func TestWorkflowFallsBackOnNotFound(t *testing.T) {
	st := newFakeStore()
	sec := &fakeSEC{errs: []error{&edgar.StatusError{Code: 404}}}
	res := runWorkflow(t, &Activities{SEC: sec, Store: st}, earnings)
	if sec.calls != 1 {
		t.Errorf("SEC asked %d times, want 1", sec.calls)
	}
	if res.Level != triage.Low || !slices.Contains(res.Reasons, "item numbers unavailable") {
		t.Errorf("got %s %q", res.Level, res.Reasons)
	}
	if _, ok := st.labels[acc]; !ok {
		t.Error("no label saved")
	}
}

// Details fetched before are reused: re-running costs SEC nothing.
func TestWorkflowReusesStoredDetails(t *testing.T) {
	st := newFakeStore()
	st.details[acc] = edgar.Details{Items: []string{"4.02"}}
	sec := &fakeSEC{}
	res := runWorkflow(t, &Activities{SEC: sec, Store: st}, earnings)
	if sec.calls != 0 || res.Level != triage.High {
		t.Errorf("%d SEC calls, level %s", sec.calls, res.Level)
	}
}

type fakeTemporal struct {
	started map[string]bool
}

func (f *fakeTemporal) ExecuteWorkflow(_ context.Context, o client.StartWorkflowOptions, _ any, _ ...any) (client.WorkflowRun, error) {
	if f.started[o.ID] {
		return nil, serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "", "")
	}
	f.started[o.ID] = true
	return nil, nil
}

func TestDispatch(t *testing.T) {
	st := newFakeStore()
	st.details["0000000003-26-000003"] = edgar.Details{Items: []string{"2.02"}}
	st.undecided = []store.Undecided{
		{AccessionNo: "0000000001-26-000001", FormType: "4"},                                       // routine: labelled on the spot
		{AccessionNo: "0000000002-26-000002", FormType: "8-K", CIK: "0000000002"},                  // tracked: gets a workflow
		{AccessionNo: "0000000003-26-000003", FormType: "8-K", CIK: "0000000003", RulesVersion: 1}, // labelled by older rules: redone from stored details
	}
	tc := &fakeTemporal{started: map[string]bool{}}
	d := &Dispatcher{Store: st, Temporal: tc, Batch: 100}

	res, err := d.Dispatch(context.Background())
	if err != nil || res != (DispatchResult{Labelled: 2, Started: 1}) {
		t.Fatalf("first run: %+v, %v", res, err)
	}
	if got := st.labels["0000000001-26-000001"]; got.Level != "routine" {
		t.Errorf("Form 4: %+v", got)
	}
	if got := st.labels["0000000003-26-000003"]; got.Level != "high" {
		t.Errorf("re-label from stored details: %+v", got)
	}
	if !tc.started[WorkflowID("0000000002-26-000002")] || tc.started[WorkflowID("0000000003-26-000003")] {
		t.Errorf("workflows started: %v", tc.started)
	}

	// The next run still finds the 8-K unlabelled, because its workflow is
	// running. Temporal refuses the second start, and that's not an error.
	st.undecided = st.undecided[1:2]
	res, err = d.Dispatch(context.Background())
	if err != nil || res != (DispatchResult{Running: 1}) {
		t.Errorf("second run: %+v, %v", res, err)
	}
}

type fakeDocs struct{ text string }

func (d fakeDocs) Document(context.Context, string, string, string) (string, error) {
	return d.text, nil
}

type fakeModel struct {
	score model.Score
	err   error
	calls int
}

func (m *fakeModel) Score(context.Context, model.Input) (model.Score, error) {
	m.calls++
	return m.score, m.err
}

const otherEvents = `Item 8.01 Other Events. On November 5, 2026, the U.S. Food and Drug Administration approved the Company's lead drug for sale in the United States.`

func modelActs(st *fakeStore, m *fakeModel) *Activities {
	return &Activities{
		SEC:      &fakeSEC{d: edgar.Details{Items: []string{"8.01"}, PrimaryDoc: "d.htm"}},
		Docs:     fakeDocs{otherEvents},
		Store:    st,
		Model:    m,
		Provider: "deepseek",
		DailyCap: 10,
		Budget:   0.20,
	}
}

// Big news filed under 8.01: the rules say low, the model raises it.
func TestWorkflowModelRaisesALabel(t *testing.T) {
	st := newFakeStore()
	m := &fakeModel{score: model.Score{Score: 85, Reason: "The FDA approved its lead drug.", Quote: "approved the Company's lead drug for sale", Model: "claude-opus-5-5"}}
	res := runWorkflow(t, modelActs(st, m), earnings)

	if res.Level != triage.High || !slices.Contains(res.Reasons, "raised by the model (score 85)") {
		t.Errorf("got %s %q", res.Level, res.Reasons)
	}
	saved := st.labels[acc]
	if saved.ModelScore == nil || *saved.ModelScore != 85 || saved.ModelQuote == "" || saved.Model != "claude-opus-5-5" {
		t.Errorf("saved %+v", saved)
	}
}

// A quote that isn't in the filing: the score is dropped, the rules stand.
func TestWorkflowDropsAnUncheckedScore(t *testing.T) {
	st := newFakeStore()
	m := &fakeModel{score: model.Score{Score: 90, Reason: "Huge news.", Quote: "the company was acquired for ten billion dollars", Model: "claude-opus-5-5"}}
	res := runWorkflow(t, modelActs(st, m), earnings)

	if res.Level != triage.Low || !slices.Contains(res.Reasons, "the model's quote wasn't found in the filing; score ignored") {
		t.Errorf("got %s %q", res.Level, res.Reasons)
	}
	if st.labels[acc].ModelScore != nil {
		t.Error("an unchecked score was saved")
	}
}

func TestWorkflowRespectsTheDailyCap(t *testing.T) {
	st := newFakeStore()
	st.modelCalls = 10
	m := &fakeModel{}
	res := runWorkflow(t, modelActs(st, m), earnings)
	if m.calls != 0 || !slices.Contains(res.Reasons, "not scored: the daily model limit was reached") {
		t.Errorf("%d model calls, reasons %q", m.calls, res.Reasons)
	}
}

// The API rejects the request for good: no retries, the rules' label stands.
func TestWorkflowSurvivesAModelRefusal(t *testing.T) {
	st := newFakeStore()
	m := &fakeModel{err: &model.PermanentError{Err: errors.New("declined")}}
	res := runWorkflow(t, modelActs(st, m), earnings)
	if m.calls != 1 || res.Level != triage.Low || !slices.Contains(res.Reasons, "model unavailable") {
		t.Errorf("%d calls, %s %q", m.calls, res.Level, res.Reasons)
	}
	if _, ok := st.labels[acc]; !ok {
		t.Error("no label saved")
	}
}

// Only 8-Ks are scored.
func TestWorkflowDoesntScoreOtherForms(t *testing.T) {
	st := newFakeStore()
	m := &fakeModel{}
	runWorkflow(t, modelActs(st, m), Filing{AccessionNo: acc, FormType: "SCHEDULE 13D/A", CIK: "0000320193"})
	if m.calls != 0 {
		t.Errorf("the model was asked about a 13D/A")
	}
}

// Every call is recorded with its cost, even when its quote then fails.
func TestScoreRecordsUsage(t *testing.T) {
	usage := model.Usage{InputTokens: 4000, OutputTokens: 120, CostUSD: 0.0014}
	for _, tc := range []struct {
		name  string
		quote string
		ran   bool
	}{
		{"checked", "approved the Company's lead drug for sale", true},
		{"quote not found", "the company was acquired", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			m := &fakeModel{score: model.Score{Score: 85, Reason: "r", Quote: tc.quote, Model: "deepseek-flash", Usage: usage}}
			res, err := modelActs(st, m).ScoreWithModel(context.Background(), earnings, edgar.Details{Items: []string{"8.01"}, PrimaryDoc: "d.htm"})
			if err != nil || res.Ran != tc.ran {
				t.Fatalf("ran %v, err %v", res.Ran, err)
			}
			want := []store.ModelCall{{AccessionNo: acc, Purpose: "score", Provider: "deepseek", Model: "deepseek-flash", InputTokens: 4000, OutputTokens: 120, CostUSD: 0.0014}}
			if !slices.Equal(st.calls, want) {
				t.Errorf("recorded %+v", st.calls)
			}
		})
	}
}

func TestScoreRespectsTheBudget(t *testing.T) {
	st := newFakeStore()
	st.spent = 0.20
	m := &fakeModel{}
	res := runWorkflow(t, modelActs(st, m), earnings)
	if m.calls != 0 || !slices.Contains(res.Reasons, "not scored: the daily model budget was reached") {
		t.Errorf("%d model calls, reasons %q", m.calls, res.Reasons)
	}
}

// A paid answer that is unusable isn't asked for again: another try would
// pay again. An account error holds model calls.
func TestScoreFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		usage model.Usage
		calls int
		held  bool
	}{
		{"paid, unusable", errors.New("model answer isn't the expected JSON"), model.Usage{InputTokens: 4000, OutputTokens: 120, CostUSD: 0.0014}, 1, false},
		{"outage", errors.New("deepseek request failed (503)"), model.Usage{}, 3, false},
		{"no balance", &model.PermanentError{Err: &model.AccountError{Err: errors.New("402")}}, model.Usage{}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			m := &fakeModel{score: model.Score{Usage: tc.usage}, err: tc.err}
			acts := modelActs(st, m)
			acts.Hold = &Hold{}
			res := runWorkflow(t, acts, earnings)
			if m.calls != tc.calls || !slices.Contains(res.Reasons, "model unavailable") || acts.Hold.Active() != tc.held {
				t.Errorf("%d calls, reasons %q, held %v", m.calls, res.Reasons, acts.Hold.Active())
			}
		})
	}
}
