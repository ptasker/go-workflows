package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/cenkalti/backoff/v4"
	"github.com/cschleiden/go-workflows/backend"
	a "github.com/cschleiden/go-workflows/internal/args"
	"github.com/cschleiden/go-workflows/internal/core"
	"github.com/cschleiden/go-workflows/internal/fn"
	"github.com/cschleiden/go-workflows/internal/history"
	"github.com/cschleiden/go-workflows/internal/metrickeys"
	"github.com/cschleiden/go-workflows/internal/tracing"
	"github.com/cschleiden/go-workflows/metrics"
	"github.com/cschleiden/go-workflows/workflow"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var ErrWorkflowCanceled = errors.New("workflow canceled")
var ErrWorkflowTerminated = errors.New("workflow terminated")

type WorkflowInstanceOptions struct {
	InstanceID string

	// FUTURE: Expose this to callers of the API. Use it only internally for now.
	// Metadata *core.WorkflowInstanceMetadata
}

type Client interface {
	CreateWorkflowInstance(ctx context.Context, options WorkflowInstanceOptions, wf workflow.Workflow, args ...interface{}) (*workflow.Instance, error)

	CancelWorkflowInstance(ctx context.Context, instance *workflow.Instance) error

	WaitForWorkflowInstance(ctx context.Context, instance *workflow.Instance, timeout time.Duration) error

	SignalWorkflow(ctx context.Context, instanceID string, name string, arg interface{}) error
}

type client struct {
	backend backend.Backend
	clock   clock.Clock
}

func New(backend backend.Backend) Client {
	return &client{
		backend: backend,
		clock:   clock.New(),
	}
}

func (c *client) CreateWorkflowInstance(ctx context.Context, options WorkflowInstanceOptions, wf workflow.Workflow, args ...interface{}) (*workflow.Instance, error) {
	// Check arguments
	if err := a.ParamsMatch(wf, args...); err != nil {
		return nil, err
	}

	inputs, err := a.ArgsToInputs(c.backend.Converter(), args...)
	if err != nil {
		return nil, fmt.Errorf("converting arguments: %w", err)
	}

	wfi := core.NewWorkflowInstance(options.InstanceID, uuid.NewString())
	metadata := &workflow.Metadata{}

	workflowName := fn.Name(wf)

	// Start new span and add to metadata
	sctx, span := c.backend.Tracer().Start(ctx, fmt.Sprintf("CreateWorkflowInstance: %s", workflowName), trace.WithAttributes(
		attribute.String(tracing.WorkflowInstanceID, wfi.InstanceID),
		attribute.String(tracing.WorkflowName, workflowName),
	))
	defer span.End()

	tracing.MarshalSpan(sctx, metadata)

	startedEvent := history.NewPendingEvent(
		c.clock.Now(),
		history.EventType_WorkflowExecutionStarted,
		&history.ExecutionStartedAttributes{
			Metadata: metadata,
			Name:     workflowName,
			Inputs:   inputs,
		})

	if err := c.backend.CreateWorkflowInstance(ctx, wfi, startedEvent); err != nil {
		return nil, fmt.Errorf("creating workflow instance: %w", err)
	}

	c.backend.Logger().Debug("Created workflow instance", "instance_id", wfi.InstanceID, "execution_id", wfi.ExecutionID)

	c.backend.Metrics().Counter(metrickeys.WorkflowInstanceCreated, metrics.Tags{}, 1)

	return wfi, nil
}

func (c *client) CancelWorkflowInstance(ctx context.Context, instance *workflow.Instance) error {
	cancellationEvent := history.NewWorkflowCancellationEvent(time.Now())
	return c.backend.CancelWorkflowInstance(ctx, instance, cancellationEvent)
}

func (c *client) SignalWorkflow(ctx context.Context, instanceID string, name string, arg interface{}) error {
	input, err := c.backend.Converter().To(arg)
	if err != nil {
		return fmt.Errorf("converting arguments: %w", err)
	}

	signalEvent := history.NewPendingEvent(
		c.clock.Now(),
		history.EventType_SignalReceived,
		&history.SignalReceivedAttributes{
			Name: name,
			Arg:  input,
		},
	)

	err = c.backend.SignalWorkflow(ctx, instanceID, signalEvent)
	if err != nil {
		return err
	}

	c.backend.Logger().Debug("Signaled workflow instance", "instance_id", instanceID)

	return nil
}

func (c *client) WaitForWorkflowInstance(ctx context.Context, instance *workflow.Instance, timeout time.Duration) error {
	if timeout == 0 {
		timeout = time.Second * 20
	}

	b := backoff.ExponentialBackOff{
		InitialInterval:     time.Millisecond * 1,
		MaxInterval:         time.Second * 1,
		Multiplier:          1.5,
		RandomizationFactor: 0.5,
		MaxElapsedTime:      timeout,
		Stop:                backoff.Stop,
		Clock:               c.clock,
	}
	b.Reset()

	ticker := backoff.NewTicker(&b)
	defer ticker.Stop()

	for range ticker.C {
		s, err := c.backend.GetWorkflowInstanceState(ctx, instance)
		if err != nil {
			return fmt.Errorf("getting workflow state: %w", err)
		}

		if s == core.WorkflowInstanceStateFinished {
			return nil
		}
	}

	return errors.New("workflow did not finish in specified timeout")
}

// GetWorkflowResult gets the workflow result for the given workflow result. It first waits for the workflow to finish or until
// the given timeout has expired.
func GetWorkflowResult[T any](ctx context.Context, c Client, instance *workflow.Instance, timeout time.Duration) (T, error) {
	if err := c.WaitForWorkflowInstance(ctx, instance, timeout); err != nil {
		return *new(T), fmt.Errorf("workflow did not finish in time: %w", err)
	}

	ic := c.(*client)
	b := ic.backend

	h, err := b.GetWorkflowInstanceHistory(ctx, instance, nil)
	if err != nil {
		return *new(T), fmt.Errorf("getting workflow history: %w", err)
	}

	// Iterate over history backwards
	for i := len(h) - 1; i >= 0; i-- {
		event := h[i]
		switch event.Type {
		case history.EventType_WorkflowExecutionFinished:
			a := event.Attributes.(*history.ExecutionCompletedAttributes)
			if a.Error != "" {
				return *new(T), errors.New(a.Error)
			}

			var r T
			if err := b.Converter().From(a.Result, &r); err != nil {
				return *new(T), fmt.Errorf("converting result: %w", err)
			}

			return r, nil

		case history.EventType_WorkflowExecutionCanceled:
			return *new(T), ErrWorkflowCanceled

		case history.EventType_WorkflowExecutionTerminated:
			return *new(T), ErrWorkflowTerminated
		}
	}

	return *new(T), errors.New("workflow finished, but could not find result event")
}
