package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/Yerassyl20036/filing-radar/internal/store"
	"github.com/Yerassyl20036/filing-radar/internal/triage"
)

// Starter starts workflows: client.Client, or a fake in tests.
type Starter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
}

// Dispatcher hands every unlabelled filing to the right place. It works from
// the table, not from what the last poll found: a filing without a label is
// unfinished work, whatever happened to it before. So a crash, a Temporal
// outage or a lost start all heal on the next run.
type Dispatcher struct {
	Store    Store
	Temporal Starter
	Batch    int // filings per run
	// NoteCap is the most notes per UTC day; 0 turns notes off (no model).
	NoteCap int
	// Budget is the daily model budget in US$. Note workflows aren't started
	// once it is spent; they would only end without a note.
	Budget float64
	// Hold is the Activities' hold on model calls. Notes started while it
	// is active would only end without a note.
	Hold *Hold

	// running holds the note workflows this dispatcher started whose note
	// row hasn't landed yet, with when they started. They count against
	// NoteCap: a running note has no row for NotesToday to count.
	running map[string]time.Time
}

// noteRunTimeout is how long a note workflow may run; after it, one that
// left no row has ended and no longer counts.
const noteRunTimeout = 2 * time.Hour

// DispatchResult is what one run did.
type DispatchResult struct {
	Labelled     int // routine forms and re-labels, done on the spot
	Started      int // workflows started
	Running      int // already had a workflow
	Failed       int
	NotesStarted int // note workflows started
	NotesRunning int // already had a note workflow
}

// Dispatch runs once. Routine forms (Form 4, 144…) are labelled at once and
// never cost a workflow. Filings whose label came from older rules are
// re-labelled from the details already stored. Everything else gets a
// workflow, at most one per filing.
func (d *Dispatcher) Dispatch(ctx context.Context) (DispatchResult, error) {
	var res DispatchResult
	todo, err := d.Store.Undecided(ctx, triage.RulesVersion, d.Batch)
	if err != nil {
		return res, err
	}

	var errs []error
	for _, u := range todo {
		var err error
		switch {
		case u.RulesVersion > 0:
			err = d.relabel(ctx, u)
			if err == nil {
				res.Labelled++
			}
		case !triage.Track(u.FormType):
			r := triage.Classify(u.FormType, nil, true)
			err = d.Store.SaveTriage(ctx, store.Triage{AccessionNo: u.AccessionNo, Level: string(r.Level), Reasons: r.Reasons, RulesVersion: triage.RulesVersion})
			if err == nil {
				res.Labelled++
			}
		default:
			var started bool
			started, err = d.start(ctx, u)
			if err == nil && started {
				res.Started++
			} else if err == nil {
				res.Running++
			}
		}
		if err != nil {
			res.Failed++
			errs = append(errs, fmt.Errorf("%s: %w", u.AccessionNo, err))
		}
	}

	notes, err := d.notes(ctx, &res)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("%d of %d failed, first: %w", len(errs), len(todo)+notes, errs[0])
	}
	return res, nil
}

// notes starts a note workflow for each high 8-K without a note, within the
// day's cap and budget. It returns how many it tried.
func (d *Dispatcher) notes(ctx context.Context, res *DispatchResult) (int, error) {
	if d.NoteCap <= 0 {
		return 0, nil
	}
	fail := func(err error) (int, error) {
		res.Failed++
		return 1, fmt.Errorf("notes: %w", err)
	}
	done, err := d.Store.NotesToday(ctx)
	if err != nil {
		return fail(err)
	}
	spent, _, err := d.Store.ModelSpendToday(ctx)
	if err != nil {
		return fail(err)
	}
	if done >= d.NoteCap || spent >= d.Budget || d.Hold.Active() {
		return 0, nil
	}
	if d.running == nil {
		d.running = map[string]time.Time{}
	}
	for acc, at := range d.running {
		if time.Since(at) > noteRunTimeout {
			delete(d.running, acc)
		}
	}
	// Ask for enough to see every running one too: new filings come first
	// and would otherwise push them out of the list.
	todo, err := d.Store.NotesToWrite(ctx, d.NoteCap-done+len(d.running))
	if err != nil {
		return fail(err)
	}
	waiting := map[string]bool{}
	for _, t := range todo {
		waiting[t.AccessionNo] = true
	}
	for acc := range d.running {
		if !waiting[acc] {
			delete(d.running, acc) // its row landed, so done counts it
		}
	}
	free := d.NoteCap - done - len(d.running)

	var errs []error
	tried := 0
	for _, t := range todo {
		_, mine := d.running[t.AccessionNo]
		if !mine && free <= 0 {
			continue
		}
		// One of ours is asked again: if it ended without a row (no answer,
		// say), this re-runs it in the slot it already holds.
		tried++
		started, err := d.startNote(ctx, t)
		switch {
		case err != nil:
			res.Failed++
			errs = append(errs, fmt.Errorf("note of %s: %w", t.AccessionNo, err))
			continue
		case started:
			res.NotesStarted++
		default:
			res.NotesRunning++
		}
		if !mine {
			d.running[t.AccessionNo] = time.Now()
			free--
		}
	}
	return tried, errors.Join(errs...)
}

func (d *Dispatcher) relabel(ctx context.Context, u store.Undecided) error {
	items, _, found, err := d.Store.Details(ctx, u.AccessionNo)
	if err != nil {
		return err
	}
	// A re-label applies new rules to stored facts only. It drops any model
	// score, which is fine while the rules are on version 1; once they change,
	// keep the score here or re-run the workflow instead.
	r := triage.Classify(u.FormType, items, found || !triage.Track(u.FormType))
	return d.Store.SaveTriage(ctx, store.Triage{AccessionNo: u.AccessionNo, Level: string(r.Level), Reasons: r.Reasons, RulesVersion: triage.RulesVersion})
}

// start returns false when the filing's workflow is already running: Temporal
// refuses the duplicate, which is the point.
func (d *Dispatcher) start(ctx context.Context, u store.Undecided) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := d.Temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        WorkflowID(u.AccessionNo),
		TaskQueue: TaskQueue,
		// Temporal refuses a second workflow while one is running. Once it has
		// finished, the label row is what says "done", and the dispatcher only
		// gets here when that row is missing, so a re-run is the right repair.
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		WorkflowRunTimeout:                       2 * time.Hour,
	}, FilingWorkflow, Filing{AccessionNo: u.AccessionNo, FormType: u.FormType, CIK: u.CIK, Company: u.Company})
	return startResult(err)
}

// startNote is start for a note workflow. A finished one leaves a note row,
// so the dispatcher only gets here when a re-run is the right repair.
func (d *Dispatcher) startNote(ctx context.Context, t store.NoteTask) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := d.Temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                                       NoteWorkflowID(t.AccessionNo),
		TaskQueue:                                TaskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		WorkflowRunTimeout:                       2 * time.Hour,
	}, NoteWorkflow, t)
	return startResult(err)
}

func startResult(err error) (bool, error) {
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &already) {
		return false, nil
	}
	return err == nil, err
}
