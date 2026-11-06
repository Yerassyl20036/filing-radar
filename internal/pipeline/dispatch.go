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
}

// DispatchResult is what one run did.
type DispatchResult struct {
	Labelled int // routine forms and re-labels, done on the spot
	Started  int // workflows started
	Running  int // already had a workflow
	Failed   int
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
	if len(errs) > 0 {
		return res, fmt.Errorf("%d of %d failed, first: %w", len(errs), len(todo), errs[0])
	}
	return res, nil
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
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &already) {
		return false, nil
	}
	return err == nil, err
}
