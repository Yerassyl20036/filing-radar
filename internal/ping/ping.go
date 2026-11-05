// Package ping is the smallest possible Temporal workflow. The worker runs it
// every minute to prove the whole path works: worker → SSH tunnel → Temporal
// server → back to an activity on this worker → result.
package ping

import (
	"context"
	"os"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is where the radar's worker listens for work.
const TaskQueue = "radar"

func Workflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Second})
	var host string
	err := workflow.ExecuteActivity(ctx, Activity).Get(ctx, &host)
	return host, err
}

// Activity returns the name of the machine that ran it.
func Activity(context.Context) (string, error) { return os.Hostname() }

// RoundTrip starts Workflow, waits for its result and says how long it took.
func RoundTrip(ctx context.Context, c client.Client) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	start := time.Now()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       "ping",
		TaskQueue:                TaskQueue,
		WorkflowExecutionTimeout: 15 * time.Second,
	}, Workflow)
	if err != nil {
		return 0, err
	}
	if err := run.Get(ctx, nil); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}
