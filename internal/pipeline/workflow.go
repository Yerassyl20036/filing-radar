// Package pipeline runs the per-filing work in Temporal. Stage 2: fetch the
// filing's details from SEC, have a model score 8-Ks, label the filing with
// the rules (raised by the model's score when it checks out), save the label.
// Later stages add steps to the same workflow: the analyst's note (3), and a
// day's wait before checking the prediction (4).
package pipeline

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
	"github.com/Yerassyl20036/filing-radar/internal/model"
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
			StartToCloseTimeout: 3 * time.Minute, // the document fetch queues for SEC, then the model thinks
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval:        30 * time.Second,
				BackoffCoefficient:     2,
				MaximumAttempts:        3,
				NonRetryableErrorTypes: []string{errModelRefused},
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
}

// Activities are the workflow's steps that talk to the outside world.
type Activities struct {
	SEC      Source // nil when EDGAR_USER_AGENT isn't set
	Docs     Documents
	Store    Store
	Model    model.Scorer // nil when ANTHROPIC_API_KEY isn't set: rules only
	DailyCap int          // most model calls per UTC day
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
	s, err := a.Model.Score(ctx, model.Input{Company: company, FormType: f.FormType, Items: d.Items, Text: text})
	var perm *model.PermanentError
	if errors.As(err, &perm) {
		return ModelResult{}, temporal.NewNonRetryableApplicationError(err.Error(), errModelRefused, err)
	}
	if err != nil {
		return ModelResult{}, err
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
