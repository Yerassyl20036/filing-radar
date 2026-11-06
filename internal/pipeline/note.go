package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
	"github.com/Yerassyl20036/filing-radar/internal/model"
	"github.com/Yerassyl20036/filing-radar/internal/store"
)

// Stage 3: the analyst's note on a high 8-K. The model compares the filing
// with the company's previous comparable one; code checks every quote, gives
// failed claims one repair, and drops what still fails. A note with fewer
// than minClaims checked claims is kept as rejected, so it isn't retried,
// and never shown.

const (
	minClaims = 2
	maxClaims = 5
	// maxNoteText is where the model stops reading a document (the model
	// package's noteDocChars). Cutting here too keeps workflow history small;
	// a quote from beyond it couldn't have been read anyway.
	maxNoteText = 60_000
)

// Note statuses, as stored, plus the one that stores nothing.
const (
	NotePublished = "published"
	NoteRejected  = "rejected"
	// NoteSkipped: no draft was paid for (the budget ran out, calls are on
	// hold, the provider didn't answer). No row is saved, so the dispatcher
	// tries again later.
	NoteSkipped = "skipped"
)

// NoteWorkflowID gives each filing one note workflow at a time.
func NoteWorkflowID(accessionNo string) string { return "note-" + accessionNo }

// NoteResult is how a note workflow ended.
type NoteResult struct {
	Status string
	Claims int    // checked claims kept
	Reason string // why it was rejected or skipped
}

// DraftResult is a model step's outcome. Skipped means the budget was
// reached and no call was made.
type DraftResult struct {
	Skipped bool
	Draft   model.Draft
	Model   string // Draft.Model doesn't survive the trip: it isn't in Draft's JSON
}

// Archive reads a filing's documents and the company's other filings:
// edgar.Client, or a fake in tests.
type Archive interface {
	Documents
	Submissions(ctx context.Context, cik string) (edgar.Submissions, error)
	FilingFiles(ctx context.Context, cik, accessionNo string) ([]edgar.File, error)
}

// NoteWorkflow writes one filing's note. It pays for at most two model
// calls, a draft and one repair, and every path that paid ends with a note
// row, so the dispatcher never pays twice for the same filing.
func NoteWorkflow(ctx workflow.Context, task store.NoteTask) (NoteResult, error) {
	var a *Activities

	gather := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Minute, // up to six SEC requests, queued behind the shared budget
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        20 * time.Second,
			BackoffCoefficient:     2,
			MaximumInterval:        5 * time.Minute,
			MaximumAttempts:        8,
			NonRetryableErrorTypes: []string{errNotFound, errNoAgent},
		},
	})
	save := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 5 * time.Second, MaximumInterval: 2 * time.Minute},
	})
	reject := func(in *model.NoteInput, modelName, reason string) (NoteResult, error) {
		n := store.Note{Status: NoteRejected, Reason: reason, Model: modelName, PromptVersion: model.PromptVersion}
		if in != nil && in.Previous != nil {
			n.PreviousAccessionNo = in.Previous.AccessionNo
		}
		err := workflow.ExecuteActivity(save, a.SaveNote, task.AccessionNo, n).Get(ctx, nil)
		return NoteResult{Status: NoteRejected, Reason: reason}, err
	}

	var in model.NoteInput
	if err := workflow.ExecuteActivity(gather, a.GatherContext, task).Get(ctx, &in); err != nil {
		if hasType(err, errNotFound) {
			return reject(nil, "", "the main document wasn't found")
		}
		// SEC trouble that outlasted the retries: no model was paid, so
		// leave no row and let the dispatcher start over later.
		return NoteResult{}, err
	}

	// Only calls that weren't billed are retried (rate limits, outages), so
	// a note pays for at most two calls: the draft and one repair.
	write := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Minute, // one call of at most two minutes, then recording it
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        30 * time.Second,
			BackoffCoefficient:     2,
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{errModelRefused, errModelAccount, errModelBilled},
		},
	})
	var dr DraftResult
	if err := workflow.ExecuteActivity(write, a.DraftNote, task.AccessionNo, in).Get(ctx, &dr); err != nil {
		// A paid answer that was unusable, or a refusal of this request:
		// trying again would most likely end the same way.
		if hasType(err, errModelBilled) || hasType(err, errModelRefused) {
			return reject(&in, "", "the model couldn't write a note: "+errMessage(err))
		}
		// Nothing was paid for (no balance, rate limits, an outage, a
		// database blip): leave no row, so the dispatcher tries again later.
		workflow.GetLogger(ctx).Warn("no draft; the note waits for a later run", "accession", task.AccessionNo, "err", err)
		return NoteResult{Status: NoteSkipped, Reason: "not written: the model didn't answer: " + errMessage(err)}, nil
	}
	if dr.Skipped {
		return NoteResult{Status: NoteSkipped, Reason: "not written: the daily model budget was reached or model calls are on hold"}, nil
	}
	d, modelName := dr.Draft, dr.Model

	// Checking is a local activity: its answer is recorded once, so a
	// change to the check can't break replay of a running workflow.
	local := workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{StartToCloseTimeout: 5 * time.Second})
	var failed []int
	if err := workflow.ExecuteLocalActivity(local, CheckNote, d, in).Get(ctx, &failed); err != nil {
		return NoteResult{}, err
	}
	if len(failed) > 0 {
		var rr DraftResult
		err := workflow.ExecuteActivity(write, a.RepairNote, task.AccessionNo, in, d, failed).Get(ctx, &rr)
		if err != nil {
			workflow.GetLogger(ctx).Warn("repair failed; dropping the failed claims", "accession", task.AccessionNo, "err", err)
		}
		if err == nil && !rr.Skipped {
			d, modelName = rr.Draft, rr.Model
			if err := workflow.ExecuteLocalActivity(local, CheckNote, d, in).Get(ctx, &failed); err != nil {
				return NoteResult{}, err
			}
		}
	}

	claims := checkedClaims(d, in, failed)
	if len(claims) < minClaims {
		return reject(&in, modelName, fmt.Sprintf("only %d checked claims", len(claims)))
	}
	// The headline isn't quote-checked like a claim. If claims were dropped
	// it may still state what they did, so the first kept claim stands in;
	// likewise if it is long or has a number no kept quote has.
	headline := d.Headline
	quotes := make([]string, len(claims))
	for i, c := range claims {
		quotes[i] = c.Quote
	}
	if len(failed) > 0 || !model.HeadlineOK(headline, quotes) {
		headline = claims[0].Text
	}
	n := store.Note{
		Status:        NotePublished,
		Headline:      headline,
		Claims:        claims,
		Model:         modelName,
		PromptVersion: model.PromptVersion,
	}
	if in.Previous != nil {
		n.PreviousAccessionNo = in.Previous.AccessionNo
	}
	if err := workflow.ExecuteActivity(save, a.SaveNote, task.AccessionNo, n).Get(ctx, nil); err != nil {
		return NoteResult{}, err
	}
	return NoteResult{Status: NotePublished, Claims: len(claims)}, nil
}

// checkedClaims keeps the claims not listed in failed, in order, at most
// maxClaims, each linked to its source document.
func checkedClaims(d model.Draft, in model.NoteInput, failed []int) []store.Claim {
	bad := make([]bool, len(d.Claims))
	for _, i := range failed {
		if i >= 0 && i < len(bad) {
			bad[i] = true
		}
	}
	var out []store.Claim
	for i, c := range d.Claims {
		if bad[i] || len(out) == maxClaims {
			continue
		}
		url := in.Current.URL
		if c.Source == "previous" && in.Previous != nil {
			url = in.Previous.URL
		}
		out = append(out, store.Claim{Text: c.Text, Quote: c.Quote, Source: c.Source, URL: url})
	}
	return out
}

// CheckNote is model.CheckClaims as a local activity.
func CheckNote(_ context.Context, d model.Draft, in model.NoteInput) ([]int, error) {
	return model.CheckClaims(d, in), nil
}

// GatherContext reads what the analyst compares: this filing's main document
// and press release, and the same for the company's previous comparable 8-K.
// Anything missing about the previous filing just means there is none.
func (a *Activities) GatherContext(ctx context.Context, task store.NoteTask) (model.NoteInput, error) {
	if a.Archive == nil {
		return model.NoteInput{}, temporal.NewNonRetryableApplicationError("EDGAR_USER_AGENT is not set", errNoAgent, nil)
	}
	company := task.Company
	if company == "" {
		company = "(unknown)"
	}
	in := model.NoteInput{Company: company, FormType: task.FormType, Items: task.Items}

	text, err := a.filingText(ctx, task.CIK, task.AccessionNo, task.PrimaryDoc)
	if is404(err) {
		return model.NoteInput{}, temporal.NewNonRetryableApplicationError(err.Error(), errNotFound, err)
	}
	if err != nil {
		return model.NoteInput{}, err
	}
	in.Current = model.Document{
		AccessionNo: task.AccessionNo,
		Label:       fmt.Sprintf("this filing (%s, %s)", task.FormType, filedOn(task.AcceptedAt)),
		URL:         edgar.DocumentURL(a.BaseURL, task.CIK, task.AccessionNo, task.PrimaryDoc),
		Text:        text,
	}

	subs, err := a.Archive.Submissions(ctx, task.CIK)
	if is404(err) {
		return in, nil
	}
	if err != nil {
		return model.NoteInput{}, err
	}
	prev, ok := subs.PreviousComparable(task.AccessionNo, task.Items)
	if !ok || prev.PrimaryDoc == "" {
		return in, nil
	}
	text, err = a.filingText(ctx, task.CIK, prev.AccessionNo, prev.PrimaryDoc)
	if is404(err) {
		return in, nil
	}
	if err != nil {
		return model.NoteInput{}, err
	}
	label := fmt.Sprintf("previous filing (%s, %s)", prev.FormType, prev.FilingDate)
	if prev.FilingDate == "" {
		label = fmt.Sprintf("previous filing (%s)", prev.FormType)
	}
	in.Previous = &model.Document{
		AccessionNo: prev.AccessionNo,
		Label:       label,
		URL:         edgar.DocumentURL(a.BaseURL, task.CIK, prev.AccessionNo, prev.PrimaryDoc),
		Text:        text,
	}
	return in, nil
}

// filingText is the main document, then the press release if the filing
// has one. The separator keeps a quote from spanning the two.
func (a *Activities) filingText(ctx context.Context, cik, accessionNo, primaryDoc string) (string, error) {
	text, err := a.Archive.Document(ctx, cik, accessionNo, primaryDoc)
	if err != nil {
		return "", err
	}
	files, err := a.Archive.FilingFiles(ctx, cik, accessionNo)
	if is404(err) {
		return cut(text), nil // no index: the main document alone
	}
	if err != nil {
		return "", err
	}
	if pr, ok := edgar.PressRelease(files); ok && pr.Name != primaryDoc {
		release, err := a.Archive.Document(ctx, cik, accessionNo, pr.Name)
		if err != nil && !is404(err) {
			return "", err
		}
		if err == nil {
			text += "\n\n----- press release (" + pr.Name + ") -----\n\n" + release
		}
	}
	return cut(text), nil
}

// cut keeps the first maxNoteText bytes, without splitting a character.
func cut(s string) string {
	if len(s) <= maxNoteText {
		return s
	}
	return strings.ToValidUTF8(s[:maxNoteText], "")
}

// eastern is SEC's time zone: a filing's date is its date in New York.
var eastern = func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.UTC
}()

func filedOn(t time.Time) string { return t.In(eastern).Format("2006-01-02") }

// DraftNote has the model write the note, unless today's budget is spent
// or model calls are on hold.
func (a *Activities) DraftNote(ctx context.Context, accessionNo string, in model.NoteInput) (DraftResult, error) {
	return a.writeNote(ctx, accessionNo, "draft", in, func() (model.Draft, error) { return a.Writer.Draft(ctx, in) })
}

// RepairNote asks once more for the claims whose quotes didn't check out.
func (a *Activities) RepairNote(ctx context.Context, accessionNo string, in model.NoteInput, d model.Draft, failed []int) (DraftResult, error) {
	return a.writeNote(ctx, accessionNo, "repair", in, func() (model.Draft, error) { return a.Writer.Repair(ctx, in, d, failed) })
}

func (a *Activities) writeNote(ctx context.Context, accessionNo, purpose string, in model.NoteInput, call func() (model.Draft, error)) (DraftResult, error) {
	if a.Writer == nil {
		return DraftResult{Skipped: true}, nil
	}
	chars := len(in.Current.Text)
	if in.Previous != nil {
		chars += len(in.Previous.Text)
	}
	var d model.Draft
	out, billed, err := a.callModel(ctx, a.Writer, chars, accessionNo, purpose, func() (string, model.Usage, error) {
		var err error
		d, err = call()
		return d.Model, d.Usage, err
	})
	if out == callSkipped {
		return DraftResult{Skipped: err == nil}, err
	}
	return DraftResult{Draft: d, Model: d.Model}, modelErr(err, billed)
}

// SaveNote stores the note. It's an upsert, so a retry is harmless. The
// accession travels apart because store.Note leaves it out of its JSON.
func (a *Activities) SaveNote(ctx context.Context, accessionNo string, n store.Note) error {
	n.AccessionNo = accessionNo
	return a.Store.SaveNote(ctx, n)
}

func is404(err error) bool {
	var se *edgar.StatusError
	return errors.As(err, &se) && se.Code == 404
}

// hasType reports whether an activity failed with the given error type.
func hasType(err error, errType string) bool {
	var ae *temporal.ApplicationError
	return errors.As(err, &ae) && ae.Type() == errType
}

// errMessage is an activity error as a reader should see it, without
// Temporal's wrapping.
func errMessage(err error) string {
	var ae *temporal.ApplicationError
	if errors.As(err, &ae) && ae.Message() != "" {
		return ae.Message()
	}
	return err.Error()
}
