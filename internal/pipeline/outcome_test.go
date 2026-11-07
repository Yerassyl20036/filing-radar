package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/Yerassyl20036/filing-radar/internal/prices"
	"github.com/Yerassyl20036/filing-radar/internal/store"
)

func (s *fakeStore) SaveOutcome(_ context.Context, o store.Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[o.AccessionNo] = o
	return nil
}

func (s *fakeStore) OutcomesToTrack(_ context.Context, limit int) ([]store.OutcomeTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toTrack[:min(limit, len(s.toTrack))], nil
}

type fakeTickers map[string]string

func (f fakeTickers) Ticker(_ context.Context, cik string) (string, error) { return f[cik], nil }

// fakePrices answers each call with the next list of closes; the last list
// repeats.
type fakePrices struct {
	mu      sync.Mutex
	answers [][]prices.Close
	err     error
	calls   int
}

func (f *fakePrices) Closes(context.Context, string, time.Time, time.Time) ([]prices.Close, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.answers[min(f.calls, len(f.answers))-1], nil
}

func nyAt(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, prices.NewYork)
}

func closeOf(y int, m time.Month, d int, price float64) prices.Close {
	at := nyAt(y, m, d, 16, 0)
	return prices.Close{Day: at.Format("2006-01-02"), At: at, Price: price}
}

// Filed Monday 2 November 2026 at 10:00 New York; Friday closed at 100.
var outcomeTask = store.OutcomeTask{AccessionNo: acc, CIK: "0000320193", AcceptedAt: nyAt(2026, 11, 2, 10, 0)}

var (
	friday = closeOf(2026, 10, 30, 100)
	monday = closeOf(2026, 11, 2, 104.2)
)

// runOutcome runs the workflow from start and returns its result and the
// workflow-clock times at which each measurement began.
func runOutcome(t *testing.T, st *fakeStore, p *fakePrices, tk fakeTickers, start time.Time) (store.Outcome, []time.Time) {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.SetStartTime(start)
	env.RegisterActivity(&Activities{Store: st, Prices: p, Tickers: tk})
	var measured []time.Time
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "MeasureOutcome" {
			measured = append(measured, env.Now())
		}
	})
	env.ExecuteWorkflow(OutcomeWorkflow, outcomeTask)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow didn't complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var o store.Outcome
	if err := env.GetWorkflowResult(&o); err != nil {
		t.Fatal(err)
	}
	return o, measured
}

// The workflow sleeps on a Temporal timer until 16:45 New York, measures
// once, and saves. The test environment skips the wait.
func TestOutcomeSleepsUntilTheClose(t *testing.T) {
	st := newFakeStore()
	p := &fakePrices{answers: [][]prices.Close{{friday, monday}}}
	o, measured := runOutcome(t, st, p, fakeTickers{"0000320193": "AAPL"}, nyAt(2026, 11, 2, 10, 5))

	wake := nyAt(2026, 11, 2, 16, 45)
	if len(measured) != 1 || measured[0].Before(wake) {
		t.Fatalf("measured at %v, want once at or after %v", measured, wake)
	}
	want := store.Outcome{AccessionNo: acc, Status: store.OutcomeScored, Ticker: "AAPL",
		PrevClose: 100, NextClose: 104.2, MovePct: 4.2, PrevDate: "2026-10-30", NextDate: "2026-11-02"}
	o.AccessionNo = acc // the result's JSON leaves it out
	if o != want || st.outcomes[acc] != want {
		t.Errorf("got %+v, saved %+v; want %+v", o, st.outcomes[acc], want)
	}
}

// A filing whose close has long passed is measured at once.
func TestOutcomeBackfillDoesNotSleep(t *testing.T) {
	st := newFakeStore()
	p := &fakePrices{answers: [][]prices.Close{{friday, monday, closeOf(2026, 11, 3, 90)}}}
	start := nyAt(2026, 11, 20, 9, 0)
	o, measured := runOutcome(t, st, p, fakeTickers{"0000320193": "AAPL"}, start)
	if len(measured) != 1 || measured[0].Sub(start) > time.Minute {
		t.Errorf("measured at %v, want at once (%v)", measured, start)
	}
	if o.Status != store.OutcomeScored || o.NextDate != "2026-11-02" {
		t.Errorf("got %+v", o)
	}
}

// A close that isn't there yet (a holiday) is asked for again a day later.
func TestOutcomeWaitsADayForAMissingClose(t *testing.T) {
	st := newFakeStore()
	tuesday := closeOf(2026, 11, 3, 97)
	p := &fakePrices{answers: [][]prices.Close{{friday}, {friday, tuesday}}}
	o, measured := runOutcome(t, st, p, fakeTickers{"0000320193": "AAPL"}, nyAt(2026, 11, 2, 10, 5))
	if len(measured) != 2 || measured[1].Sub(measured[0]) < 24*time.Hour {
		t.Fatalf("measured at %v, want twice a day apart", measured)
	}
	if o.Status != store.OutcomeScored || o.NextDate != "2026-11-03" || o.MovePct != -3 {
		t.Errorf("got %+v", o)
	}
}

func TestOutcomeGivesUpAfterFiveDays(t *testing.T) {
	st := newFakeStore()
	p := &fakePrices{answers: [][]prices.Close{{friday}}}
	o, measured := runOutcome(t, st, p, fakeTickers{"0000320193": "AAPL"}, nyAt(2026, 11, 2, 10, 5))
	if len(measured) != 1+priceDays {
		t.Errorf("measured %d times, want %d", len(measured), 1+priceDays)
	}
	if o.Status != store.OutcomeNoPrice || st.outcomes[acc].Status != store.OutcomeNoPrice || o.Ticker != "AAPL" {
		t.Errorf("got %+v", o)
	}
}

func TestOutcomeWithoutTickerOrPrices(t *testing.T) {
	st := newFakeStore()
	o, _ := runOutcome(t, st, &fakePrices{}, fakeTickers{}, nyAt(2026, 11, 2, 10, 5))
	if o.Status != store.OutcomeNoTicker || st.outcomes[acc].Status != store.OutcomeNoTicker {
		t.Errorf("no ticker: got %+v", o)
	}

	st = newFakeStore()
	p := &fakePrices{err: fmt.Errorf("GONE: %w", prices.ErrUnknownTicker)}
	o, _ = runOutcome(t, st, p, fakeTickers{"0000320193": "GONE"}, nyAt(2026, 11, 2, 10, 5))
	if o.Status != store.OutcomeNoPrice || o.Ticker != "GONE" {
		t.Errorf("unknown ticker: got %+v", o)
	}
}

// A source that always refuses the ticker with a 400 is "no price" at
// once; one that always fails otherwise is "no price" after the daily
// retries. Either way a row is saved, so the dispatcher's list moves on.
func TestOutcomePersistentFailuresSaveARow(t *testing.T) {
	st := newFakeStore()
	p := &fakePrices{err: &prices.StatusError{Code: 400}}
	o, measured := runOutcome(t, st, p, fakeTickers{"0000320193": "BAD.X"}, nyAt(2026, 11, 2, 10, 5))
	if len(measured) != 1 || o.Status != store.OutcomeNoPrice || st.outcomes[acc].Status != store.OutcomeNoPrice {
		t.Errorf("400: measured %d times, got %+v", len(measured), o)
	}

	st = newFakeStore()
	p = &fakePrices{err: &prices.StatusError{Code: 503}}
	o, _ = runOutcome(t, st, p, fakeTickers{"0000320193": "AAPL"}, nyAt(2026, 11, 2, 10, 5))
	if o.Status != store.OutcomeNoPrice || st.outcomes[acc].Status != store.OutcomeNoPrice {
		t.Errorf("503: got %+v, saved %+v", o, st.outcomes[acc])
	}
	if p.calls != (1+priceDays)*10 {
		t.Errorf("503: %d price calls, want %d (10 attempts a day)", p.calls, (1+priceDays)*10)
	}
}

func TestRejected(t *testing.T) {
	for code, want := range map[int]bool{400: true, 422: true, 401: false, 403: false, 429: false, 500: false} {
		if got := rejected(fmt.Errorf("x: %w", &prices.StatusError{Code: code})); got != want {
			t.Errorf("rejected(%d) = %v, want %v", code, got, want)
		}
	}
	if rejected(errors.New("timeout")) {
		t.Error("rejected(timeout) = true")
	}
}

func TestOutcomeWakeAt(t *testing.T) {
	got, _ := OutcomeWakeAt(context.Background(), nyAt(2026, 11, 6, 18, 0)) // Friday evening
	if want := nyAt(2026, 11, 9, 16, 45); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDispatchOutcomes(t *testing.T) {
	st := newFakeStore()
	st.toTrack = []store.OutcomeTask{{AccessionNo: "a"}, {AccessionNo: "b"}}
	tc := &fakeTemporal{started: map[string]bool{}}
	d := &Dispatcher{Store: st, Temporal: tc, Batch: 100}
	if res, err := d.Dispatch(context.Background()); err != nil || res != (DispatchResult{}) {
		t.Fatalf("outcomes off: %+v, %v", res, err)
	}
	d.Outcomes = true
	if res, err := d.Dispatch(context.Background()); err != nil || res.OutcomesStarted != 2 || !tc.started[OutcomeWorkflowID("a")] {
		t.Fatalf("first run: %+v, %v", res, err)
	}
	// Still no rows: they're asleep until the close, and Temporal says so.
	if res, err := d.Dispatch(context.Background()); err != nil || res.OutcomesStarted != 0 || res.OutcomesRunning != 2 {
		t.Fatalf("second run: %+v, %v", res, err)
	}
}

func TestTickerCache(t *testing.T) {
	fetches := 0
	fail := false
	c := &TickerCache{TTL: time.Hour, Fetch: func(context.Context) (map[string]string, error) {
		fetches++
		if fail {
			return nil, errors.New("SEC is down")
		}
		return map[string]string{"0000320193": "AAPL"}, nil
	}}
	ctx := context.Background()
	for range 3 {
		if tk, err := c.Ticker(ctx, "0000320193"); err != nil || tk != "AAPL" {
			t.Fatalf("got %q, %v", tk, err)
		}
	}
	if tk, _ := c.Ticker(ctx, "0000000001"); tk != "" || fetches != 1 {
		t.Errorf("unknown company: %q after %d fetches", tk, fetches)
	}

	// Expired and SEC down: the old list still answers.
	c.at = time.Now().Add(-2 * time.Hour)
	fail = true
	if tk, err := c.Ticker(ctx, "0000320193"); err != nil || tk != "AAPL" || fetches != 2 {
		t.Errorf("stale: %q, %v after %d fetches", tk, err, fetches)
	}
	if _, err := (&TickerCache{TTL: time.Hour, Fetch: c.Fetch}).Ticker(ctx, "x"); err == nil {
		t.Error("no list at all: no error")
	}
}
