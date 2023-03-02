package workflow

import (
	"time"

	"github.com/cschleiden/go-workflows/internal/command"
	"github.com/cschleiden/go-workflows/internal/converter"
	"github.com/cschleiden/go-workflows/internal/sync"
	"github.com/cschleiden/go-workflows/internal/workflowstate"
	"github.com/cschleiden/go-workflows/internal/workflowtracer"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func ScheduleTimer(ctx Context, delay time.Duration) Future[struct{}] {
	f := sync.NewFuture[struct{}]()

	// If the context is already canceled, return immediately.
	if ctx.Err() != nil {
		f.Set(struct{}{}, ctx.Err())
		return f
	}

	wfState := workflowstate.WorkflowState(ctx)

	scheduleEventID := wfState.GetNextScheduleEventID()
	at := Now(ctx).Add(delay)
	timerCmd := command.NewScheduleTimerCommand(scheduleEventID, at)
	wfState.AddCommand(timerCmd)

	wfState.TrackFuture(scheduleEventID, workflowstate.AsDecodingSettable(converter.GetConverter(ctx), f))

	ctx, span := workflowtracer.Tracer(ctx).Start(ctx, "ScheduleTimer",
		trace.WithAttributes(
			attribute.Int64("duration_ms", int64(delay/time.Millisecond)),
			attribute.String("now", Now(ctx).String()),
			attribute.String("at", at.String()),
		))
	defer span.End()

	// Check if the context is cancelable
	if c, cancelable := ctx.Done().(sync.CancelChannel); cancelable {
		// Register a callback for when it's canceled. The only operation on the `Done` channel
		// is that it's closed when the context is canceled.
		canceled := false

		c.AddReceiveCallback(func(v struct{}, ok bool) {
			// Ignore any future cancelation events for this timer
			if canceled {
				return
			}
			canceled = true

			timerCmd.Cancel()

			// Remove the timer future from the workflow state and mark it as canceled if it hasn't already fired. This is different
			// from subworkflow behavior, where we want to wait for the subworkflow to complete before proceeding. Here we can
			// continue right away.
			if fi, ok := f.(sync.FutureInternal[struct{}]); ok {
				if !fi.Ready() {
					wfState.RemoveFuture(scheduleEventID)
					f.Set(v, sync.Canceled)
				}
			}
		})
	}

	return f
}
