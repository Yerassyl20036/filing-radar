package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Yerassyl20036/filing-radar/internal/prices"
	"github.com/Yerassyl20036/filing-radar/internal/store"
)

// Stage 4, the scorecard. For every 8-K the model scored, wait until the
// first market close after EDGAR accepted it, then record how far the stock
// moved from the close before. No model is called: this checks the model.

const (
	// closeSettle is how long after 16:00 New York the workflow wakes: the
	// day's close takes a few minutes to settle at the price source.
	closeSettle = 45 * time.Minute
	// priceDays is how many daily retries a missing close gets (a holiday,
	// a halt, a late source) before the outcome is "no price".
	priceDays = 5
	// priceWindow is how far around the filing closes are asked for: enough
	// to span a long weekend and a holiday on both sides.
	priceWindow = 10 * 24 * time.Hour
)

// outcomePending means the close after the filing isn't there yet. It is
// never saved; the workflow waits a day and asks again.
const outcomePending = "pending"

// OutcomeWorkflowID gives each filing one outcome workflow at a time.
func OutcomeWorkflowID(accessionNo string) string { return "outcome-" + accessionNo }

// Tickers finds a company's ticker by its 10-digit CIK; "" when it has none.
type Tickers interface {
	Ticker(ctx context.Context, cik string) (string, error)
}

// OutcomeWorkflow sleeps until the first close after the filing, then
// measures the move and saves it. The sleep is a Temporal timer: it is
// stored on the server, so it survives worker restarts and deploys, which a
// time.Sleep in a goroutine would not. A filing whose close has passed
// (a backfill) doesn't sleep at all.
func OutcomeWorkflow(ctx workflow.Context, task store.OutcomeTask) (store.Outcome, error) {
	var a *Activities

	// The close time comes from a local activity, not workflow code: it
	// reads the time zone database, and its recorded answer is what a
	// replay uses.
	local := workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{StartToCloseTimeout: 5 * time.Second})
	var wake time.Time
	if err := workflow.ExecuteLocalActivity(local, OutcomeWakeAt, task.AcceptedAt).Get(ctx, &wake); err != nil {
		return store.Outcome{}, err
	}
	if d := wake.Sub(workflow.Now(ctx)); d > 0 {
		if err := workflow.Sleep(ctx, d); err != nil {
			return store.Outcome{}, err
		}
	}

	measure := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		// One price request, but behind the price client's one-per-second
		// queue: a backfill of outcomeBatch filings waits up to that many
		// seconds there, and the wait must not count as a source failure.
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Minute,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Hour,
			MaximumAttempts:    10,
		},
	})
	save := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 5 * time.Second, MaximumInterval: 2 * time.Minute},
	})

	var o store.Outcome
	ticker := ""
	for day := 0; ; day++ {
		o = store.Outcome{}
		if err := workflow.ExecuteActivity(measure, a.MeasureOutcome, task).Get(ctx, &o); err != nil {
			var appErr *temporal.ApplicationError
			if errors.As(err, &appErr) && appErr.NonRetryable() {
				return store.Outcome{}, err // misconfigured worker: waiting won't help
			}
			// Every retry failed. Count it as a missing day rather than
			// ending without a row: a workflow with no row is restarted by
			// the dispatcher at the front of its oldest-first list, and a
			// ticker that always fails would hold that place for good.
			workflow.GetLogger(ctx).Warn("prices unavailable; trying again tomorrow", "accession", task.AccessionNo, "err", err)
			o.Status = outcomePending
		}
		if o.Ticker != "" {
			ticker = o.Ticker
		}
		if o.Status != outcomePending {
			break
		}
		if day >= priceDays {
			o = store.Outcome{AccessionNo: task.AccessionNo, Status: store.OutcomeNoPrice, Ticker: ticker}
			break
		}
		if err := workflow.Sleep(ctx, 24*time.Hour); err != nil {
			return store.Outcome{}, err
		}
	}
	err := workflow.ExecuteActivity(save, a.SaveOutcome, task.AccessionNo, o).Get(ctx, nil)
	return o, err
}

// OutcomeWakeAt is when the outcome workflow should look for prices: a
// while after the first close after acceptance.
func OutcomeWakeAt(_ context.Context, acceptedAt time.Time) (time.Time, error) {
	return prices.NextClose(acceptedAt).Add(closeSettle).UTC(), nil
}

// MeasureOutcome finds the filing's ticker and the closes around it. A
// company with no ticker, or a ticker with no prices, is an answer, not an
// error; so is "not yet", which the workflow waits out.
func (a *Activities) MeasureOutcome(ctx context.Context, task store.OutcomeTask) (store.Outcome, error) {
	o := store.Outcome{AccessionNo: task.AccessionNo}
	if a.Tickers == nil || a.Prices == nil {
		return o, temporal.NewNonRetryableApplicationError("no ticker or price source configured", errNoAgent, nil)
	}
	ticker, err := a.Tickers.Ticker(ctx, task.CIK)
	if err != nil {
		return o, err
	}
	if ticker == "" {
		o.Status = store.OutcomeNoTicker
		return o, nil
	}
	o.Ticker = ticker

	closes, err := a.Prices.Closes(ctx, ticker, task.AcceptedAt.Add(-priceWindow), task.AcceptedAt.Add(priceWindow))
	if errors.Is(err, prices.ErrUnknownTicker) || rejected(err) {
		o.Status = store.OutcomeNoPrice
		return o, nil
	}
	if err != nil {
		return o, err
	}
	before, after := prices.Around(closes, task.AcceptedAt)
	switch {
	case before == nil:
		// Nothing to compare with (listed after the filing, or a long
		// halt); waiting won't bring an earlier close.
		o.Status = store.OutcomeNoPrice
	case after == nil:
		o.Status = outcomePending
	default:
		o.Status = store.OutcomeScored
		o.PrevClose, o.PrevDate = before.Price, before.Day
		o.NextClose, o.NextDate = after.Price, after.Day
		o.MovePct = math.Round((after.Price-before.Price)/before.Price*100*10000) / 10000
	}
	return o, nil
}

// rejected reports a client error that asking again won't fix, such as a
// 400 for a symbol format the source refuses. 401, 403, 408 and 429 are
// left out: they say we are blocked or too fast, not that the ticker is bad.
func rejected(err error) bool {
	var se *prices.StatusError
	if !errors.As(err, &se) || se.Code < 400 || se.Code >= 500 {
		return false
	}
	switch se.Code {
	case 401, 403, 408, 429:
		return false
	}
	return true
}

// SaveOutcome stores the outcome. It is an upsert, so a retry is harmless.
// The accession number travels apart: Outcome leaves it out of its JSON.
func (a *Activities) SaveOutcome(ctx context.Context, accessionNo string, o store.Outcome) error {
	o.AccessionNo = accessionNo
	return a.Store.SaveOutcome(ctx, o)
}

// TickerCache keeps SEC's ticker list for TTL, so the list (about 1 MB) is
// fetched about once a day, not once per filing. If a refresh fails, the old
// list is used for another hour.
type TickerCache struct {
	Fetch func(ctx context.Context) (map[string]string, error)
	TTL   time.Duration

	mu sync.Mutex
	m  map[string]string
	at time.Time
}

// Ticker returns the company's ticker, or "" when SEC lists none.
func (c *TickerCache) Ticker(ctx context.Context, cik string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || time.Since(c.at) > c.TTL {
		m, err := c.Fetch(ctx)
		switch {
		case err == nil:
			c.m, c.at = m, time.Now()
		case c.m == nil:
			return "", err
		default:
			// Try again in an hour rather than on every filing.
			c.at = time.Now().Add(time.Hour - c.TTL)
			slog.Warn("ticker list refresh failed; using the old one", "err", err)
		}
	}
	return c.m[cik], nil
}
