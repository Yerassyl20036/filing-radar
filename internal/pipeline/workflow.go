// Package pipeline runs the per-filing work in Temporal. Stage 2: fetch the
// filing's details from SEC, have a model score 8-Ks, label the filing with
// the rules (raised by the model's score when it checks out), save the label.
// Stage 3's analyst note has its own workflow (note.go), started once the
// filing is labelled high. Stage 4's outcome workflow (outcome.go) waits for
// the next market close after each scored 8-K and records the move.
package pipeline

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
	"github.com/Yerassyl20036/filing-radar/internal/model"
	"github.com/Yerassyl20036/filing-radar/internal/prices"
	"github.com/Yerassyl20036/filing-radar/internal/store"
	"github.com/Yerassyl20036/filing-radar/internal/triage"
)

// TaskQueue is shared with the health-check ping: one worker serves both.
const TaskQueue = "radar"

// Filing is the workflow's input: just enough to find the filing at SEC.
type Filing struct {
	AccessionNo string
	FormType    string
	CIK         string // the company's, 10 digits
	Company     string
}

// WorkflowID makes the accession number the workflow's identity, so Temporal
// itself refuses to run two workflows for one filing.
func WorkflowID(accessionNo string) string { return "filing-" + accessionNo }

// FilingWorkflow labels one filing. It always ends with a label saved: if SEC
// can't give the details after about 20 minutes of retries, the label rests
// on the form alone and says so; if the model can't help, the rules' label
// stands. Nothing leaves a filing pending for good.
func FilingWorkflow(ctx workflow.Context, f Filing) (triage.Result, error) {
	var a *Activities // only the method names matter here; the worker holds the real one

	fetch := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute, // includes queueing for the shared 1-per-second SEC budget
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        20 * time.Second,
			BackoffCoefficient:     2,
			MaximumInterval:        5 * time.Minute,
			MaximumAttempts:        8,
			NonRetryableErrorTypes: []string{errNotFound, errNoAgent},
		},
	})
	var d edgar.Details
	found := true
	if err := workflow.ExecuteActivity(fetch, a.FetchDetails, f).Get(ctx, &d); err != nil {
		workflow.GetLogger(ctx).Warn("details unavailable; labelling from the form", "accession", f.AccessionNo, "err", err)
		found = false
	}

	// Only 8-Ks are scored: they're where news hides under dull item numbers.
	var m ModelResult
	if found && d.PrimaryDoc != "" && isEightK(f.FormType) {
		score := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			// The document fetch queues for SEC, then one model call of at
			// most two minutes. Only calls that weren't billed are retried.
			StartToCloseTimeout: 4 * time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval:        30 * time.Second,
				BackoffCoefficient:     2,
				MaximumAttempts:        3,
				NonRetryableErrorTypes: []string{errModelRefused, errModelAccount, errModelBilled},
			},
		})
		if err := workflow.ExecuteActivity(score, a.ScoreWithModel, f, d).Get(ctx, &m); err != nil {
			workflow.GetLogger(ctx).Warn("model unavailable; the rules' label stands", "accession", f.AccessionNo, "err", err)
			m = ModelResult{Note: "model unavailable"}
		}
	}

	// The rules run as a local activity, not as workflow code: workflow code is
	// replayed after a restart and must behave the same every time, but rules
	// change between versions. A local activity's result is recorded once.
	local := workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{StartToCloseTimeout: 5 * time.Second})
	var res triage.Result
	if err := workflow.ExecuteLocalActivity(local, Classify, f.FormType, d.Items, found, m).Get(ctx, &res); err != nil {
		return res, err
	}

	// Saving retries without limit: the label must land, and a database outage
	// is worth waiting out.
	save := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 5 * time.Second, MaximumInterval: 2 * time.Minute},
	})
	err := workflow.ExecuteActivity(save, a.SaveTriage, f.AccessionNo, res, m).Get(ctx, nil)
	return res, err
}

func isEightK(form string) bool { return form == "8-K" || form == "8-K/A" }

// Error types the fetch shouldn't retry.
const (
	errNotFound     = "NotFound"     // SEC has no JSON for this company
	errNoAgent      = "NoAgent"      // EDGAR_USER_AGENT isn't set, so we may not ask
	errModelRefused = "ModelRefused" // the API rejected the request or the model declined
	errModelAccount = "ModelAccount" // no balance, a bad key: no call works until someone fixes it
	errModelBilled  = "ModelBilled"  // the call was paid for but its answer is unusable; another try would pay again
)

// Source is where details come from: edgar.Client, or a fake in tests.
type Source interface {
	Details(ctx context.Context, cik, accessionNo string) (edgar.Details, error)
}

// Documents fetches a filing's main document as text: edgar.Client, or a fake.
type Documents interface {
	Document(ctx context.Context, cik, accessionNo, primaryDoc string) (string, error)
}

// Store is where facts and labels go: store.Store, or a fake in tests.
type Store interface {
	Details(ctx context.Context, accessionNo string) (items []string, primaryDoc string, found bool, err error)
	SaveDetails(ctx context.Context, accessionNo string, items []string, primaryDoc string) error
	SaveTriage(ctx context.Context, t store.Triage) error
	Undecided(ctx context.Context, rulesVersion, limit int) ([]store.Undecided, error)
	ModelCallsToday(ctx context.Context) (int, error)
	RecordModelCall(ctx context.Context, c store.ModelCall) error
	ModelSpendToday(ctx context.Context) (usd float64, calls int, err error)
	SaveNote(ctx context.Context, n store.Note) error
	NotesToWrite(ctx context.Context, limit int) ([]store.NoteTask, error)
	NotesToday(ctx context.Context) (int, error)
	SaveOutcome(ctx context.Context, o store.Outcome) error
	OutcomesToTrack(ctx context.Context, limit int) ([]store.OutcomeTask, error)
}

// Model is what one provider client does: score 8-Ks and draft notes.
type Model interface {
	model.Scorer
	model.Writer
}

// Activities are the workflows' steps that talk to the outside world.
type Activities struct {
	SEC      Source // nil when EDGAR_USER_AGENT isn't set
	Docs     Documents
	Archive  Archive // the note's documents; nil when EDGAR_USER_AGENT isn't set
	BaseURL  string  // sec.gov, for the links in notes; empty means edgar.DefaultBaseURL
	Store    Store
	Model    model.Scorer  // nil when no model API key is set: rules only
	Writer   model.Writer  // nil when no model API key is set: no notes
	Provider string        // as recorded with each call: "deepseek", "anthropic"...
	Tickers  Tickers       // SEC's ticker list; nil when EDGAR_USER_AGENT isn't set
	Prices   prices.Source // daily closes, for stage 4
	DailyCap int           // most model scores per UTC day
	// Budget is the most the models may cost per UTC day, in US$. The
	// balance is prepaid with no top-up, so it is checked before every call.
	Budget float64
	// Hold pauses model calls after an account error; shared with the
	// Dispatcher. Nil means calls are never held.
	Hold *Hold

	mu sync.Mutex
	// inFlight is the worst-case cost of calls running now. Up to eight
	// activities run at once, and without it all of them could pass the
	// same budget check.
	inFlight float64
}

// Hold pauses model calls for a while after the provider refused the
// account (no balance, a bad key): every call would fail until someone
// tops up or fixes the key. It is its own type because Temporal registers
// every exported method of Activities as an activity.
type Hold struct {
	mu    sync.Mutex
	until time.Time
}

// accountHold is how long model calls pause after an account error.
const accountHold = time.Hour

// Active reports whether calls are paused now. A nil Hold never is.
func (h *Hold) Active() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return time.Now().Before(h.until)
}

func (h *Hold) start(d time.Duration) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.until = time.Now().Add(d)
}

// overBudget reports whether today's model spend, with calls in flight, has
// reached the budget. It is a cheap early check; reserve is the real one.
func (a *Activities) overBudget(ctx context.Context) (bool, error) {
	spent, _, err := a.Store.ModelSpendToday(ctx)
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return spent+a.inFlight >= a.Budget, nil
}

// reserve holds a call's worst-case cost against today's budget until
// release is called, after the real cost is recorded. ok is false when the
// budget, counting calls in flight, has no room for this one.
func (a *Activities) reserve(ctx context.Context, est float64) (release func(), ok bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	spent, _, err := a.Store.ModelSpendToday(ctx)
	if err != nil {
		return nil, false, err
	}
	if total := spent + a.inFlight; total >= a.Budget || total+est > a.Budget {
		return nil, false, nil
	}
	a.inFlight += est
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.inFlight -= est
	}, true, nil
}

// callOutcome is how a model call went, for the activity to report.
type callOutcome int

const (
	callSkipped callOutcome = iota // held or no budget: nothing was sent
	callMade
)

// callModel makes one paid call within the budget and records what it
// cost. billed is true when the provider charged, or may have: a call cut
// off by a timeout is counted at its estimate, so the budget errs safe.
func (a *Activities) callModel(ctx context.Context, impl any, promptChars int, accessionNo, purpose string,
	call func() (modelName string, u model.Usage, err error)) (out callOutcome, billed bool, err error) {
	if a.Hold.Active() {
		return callSkipped, false, nil
	}
	var est model.Usage
	if e, ok := impl.(model.Estimator); ok {
		est = e.Estimate(promptChars)
	}
	release, ok, err := a.reserve(ctx, est.CostUSD)
	if err != nil || !ok {
		return callSkipped, false, err
	}
	defer release()

	name, u, err := call()
	billed = u.InputTokens > 0 || u.OutputTokens > 0
	if err != nil && !billed && cutOff(err) {
		name, u, billed = "(cut off; cost estimated)", est, true
	}
	if billed || err == nil {
		a.record(ctx, accessionNo, purpose, name, u)
	}
	var acct *model.AccountError
	if errors.As(err, &acct) {
		a.Hold.start(accountHold)
	}
	return callMade, billed, err
}

// cutOff reports whether a call failed after it may have reached the
// provider: a timeout or a cancel while waiting for the answer.
func cutOff(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		(errors.As(err, &ne) && ne.Timeout())
}

// record stores what a call cost. It uses its own short context, so an
// activity cut off by its timeout still records the call it paid for. A
// failure to record is logged, not returned: retrying would pay again.
func (a *Activities) record(ctx context.Context, accessionNo, purpose, modelName string, u model.Usage) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	c := store.ModelCall{
		AccessionNo: accessionNo, Purpose: purpose, Provider: a.Provider, Model: modelName,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CostUSD: u.CostUSD,
	}
	if rerr := a.Store.RecordModelCall(rctx, c); rerr != nil {
		activity.GetLogger(ctx).Warn("model call not recorded; today's spend is undercounted", "accession", accessionNo, "purpose", purpose, "err", rerr)
	}
}

// modelErr tells Temporal which model failures not to retry: a paid call
// (asking again pays again), an account problem, a permanent refusal.
func modelErr(err error, billed bool) error {
	var acct *model.AccountError
	var perm *model.PermanentError
	switch {
	case err == nil:
		return nil
	case billed:
		return temporal.NewNonRetryableApplicationError(err.Error(), errModelBilled, err)
	case errors.As(err, &acct):
		return temporal.NewNonRetryableApplicationError(err.Error(), errModelAccount, err)
	case errors.As(err, &perm):
		return temporal.NewNonRetryableApplicationError(err.Error(), errModelRefused, err)
	}
	return err
}

// ModelResult is what the model step decided. Ran is false when the model
// was off, over its cap, or its quote didn't check out; Note says why, when
// a reader should know.
type ModelResult struct {
	Ran    bool
	Score  int
	Reason string
	Quote  string
	Model  string
	Note   string
}

// FetchDetails gets a filing's items and main document, from our own table if
// we've fetched them before, otherwise from SEC, and stores them.
func (a *Activities) FetchDetails(ctx context.Context, f Filing) (edgar.Details, error) {
	items, doc, found, err := a.Store.Details(ctx, f.AccessionNo)
	if err != nil {
		return edgar.Details{}, err
	}
	if found {
		return edgar.Details{Items: items, PrimaryDoc: doc}, nil
	}
	if a.SEC == nil {
		return edgar.Details{}, temporal.NewNonRetryableApplicationError("EDGAR_USER_AGENT is not set", errNoAgent, nil)
	}

	d, err := a.SEC.Details(ctx, f.CIK, f.AccessionNo)
	var se *edgar.StatusError
	if errors.As(err, &se) && se.Code == 404 {
		return edgar.Details{}, temporal.NewNonRetryableApplicationError(err.Error(), errNotFound, err)
	}
	if err != nil {
		return edgar.Details{}, err // ErrNotListed, 403, 429, timeouts: worth another try
	}
	if err := a.Store.SaveDetails(ctx, f.AccessionNo, d.Items, d.PrimaryDoc); err != nil {
		return edgar.Details{}, err
	}
	return d, nil
}

// ScoreWithModel reads the 8-K's main document and asks the model about it.
// Being off, over the cap, or unable to back up its answer are all normal
// outcomes, not errors: the rules' label stands and Note says why.
func (a *Activities) ScoreWithModel(ctx context.Context, f Filing, d edgar.Details) (ModelResult, error) {
	if a.Model == nil || a.Docs == nil {
		return ModelResult{}, nil
	}
	n, err := a.Store.ModelCallsToday(ctx)
	if err != nil {
		return ModelResult{}, err
	}
	if n >= a.DailyCap {
		return ModelResult{Note: "not scored: the daily model limit was reached"}, nil
	}
	budgetNote := ModelResult{Note: "not scored: the daily model budget was reached"}
	if a.Hold.Active() {
		return ModelResult{Note: "not scored: the model account needs attention"}, nil
	}
	// Checked again, with a reservation, once the document's size is known;
	// this one spares SEC a fetch when the budget is already gone.
	if over, err := a.overBudget(ctx); err != nil {
		return ModelResult{}, err
	} else if over {
		return budgetNote, nil
	}

	text, err := a.Docs.Document(ctx, f.CIK, f.AccessionNo, d.PrimaryDoc)
	var se *edgar.StatusError
	if errors.As(err, &se) && se.Code == 404 {
		return ModelResult{Note: "not scored: the main document wasn't found"}, nil
	}
	if err != nil {
		return ModelResult{}, err
	}

	company := f.Company
	if company == "" {
		company = "(unknown)"
	}
	var s model.Score
	out, billed, err := a.callModel(ctx, a.Model, len(text), f.AccessionNo, "score", func() (string, model.Usage, error) {
		var err error
		s, err = a.Model.Score(ctx, model.Input{Company: company, FormType: f.FormType, Items: d.Items, Text: text})
		return s.Model, s.Usage, err
	})
	if out == callSkipped && err == nil {
		if a.Hold.Active() {
			return ModelResult{Note: "not scored: the model account needs attention"}, nil
		}
		return budgetNote, nil
	}
	if err != nil {
		return ModelResult{}, modelErr(err, billed)
	}
	if !model.Verify(s.Quote, text) {
		return ModelResult{Model: s.Model, Note: "the model's quote wasn't found in the filing; score ignored"}, nil
	}
	return ModelResult{Ran: true, Score: s.Score, Reason: s.Reason, Quote: s.Quote, Model: s.Model}, nil
}

// SaveTriage stores the label. Saving is an upsert, so a retry is harmless.
func (a *Activities) SaveTriage(ctx context.Context, accessionNo string, r triage.Result, m ModelResult) error {
	t := store.Triage{
		AccessionNo:  accessionNo,
		Level:        string(r.Level),
		Reasons:      r.Reasons,
		RulesVersion: triage.RulesVersion,
		Model:        m.Model,
	}
	if m.Ran {
		score := m.Score
		t.ModelScore, t.ModelReason, t.ModelQuote = &score, m.Reason, m.Quote
	}
	return a.Store.SaveTriage(ctx, t)
}

// Classify runs the rules, then lets a checked model score raise the label.
// It's a local activity so rule changes can't break workflow replay.
func Classify(_ context.Context, form string, items []string, detailsFound bool, m ModelResult) (triage.Result, error) {
	r := triage.Classify(form, items, detailsFound)
	if m.Ran {
		r = triage.Raise(r, m.Score)
	}
	if m.Note != "" {
		r.Reasons = append(r.Reasons, m.Note)
	}
	return r, nil
}
