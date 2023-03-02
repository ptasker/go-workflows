package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/cschleiden/go-workflows/internal/core"
	"github.com/cschleiden/go-workflows/internal/history"
	"github.com/cschleiden/go-workflows/internal/task"
	"github.com/cschleiden/go-workflows/internal/tracing"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Find all due future events. For each event:
// - Look up event data
// - Add to pending event stream for workflow instance
// - Try to queue workflow task for workflow instance
// - Remove event from future event set and delete event data
//
// KEYS[1] - future event set key
// KEYS[2] - workflow task queue stream
// KEYS[3] - workflow task queue set
// ARGV[1] - current timestamp for zrange
//
// Note: this does not work with Redis Cluster since not all keys are passed into the script.
var futureEventsCmd = redis.NewScript(`
	-- Find events which should become visible now
	local events = redis.call("ZRANGE", KEYS[1], "-inf", ARGV[1], "BYSCORE")
	for i = 1, #events do
		local instanceID = redis.call("HGET", events[i], "instance")

		-- Add event to pending event stream
		local eventData = redis.call("HGET", events[i], "event")
		local pending_events_key = "pending-events:" .. instanceID
		redis.call("XADD", pending_events_key, "*", "event", eventData)

		-- Try to queue workflow task
		local already_queued = redis.call("SADD", KEYS[3], instanceID)
		if already_queued ~= 0 then
			redis.call("XADD", KEYS[2], "*", "id", instanceID, "data", "")
		end

		-- Delete event hash data
		redis.call("DEL", events[i])
		redis.call("ZREM", KEYS[1], events[i])
	end

	return #events
`)

func (rb *redisBackend) GetWorkflowTask(ctx context.Context) (*task.Workflow, error) {
	// Check for future events
	now := time.Now().UnixMilli()
	nowStr := strconv.FormatInt(now, 10)

	queueKeys := rb.workflowQueue.Keys()

	if _, err := futureEventsCmd.Run(ctx, rb.rdb, []string{
		futureEventsKey(),
		queueKeys.StreamKey,
		queueKeys.SetKey,
	}, nowStr).Result(); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("checking future events: %w", err)
	}

	// Try to get a workflow task, this locks the instance when it dequeues one
	instanceTask, err := rb.workflowQueue.Dequeue(ctx, rb.rdb, rb.options.WorkflowLockTimeout, rb.options.BlockTimeout)
	if err != nil {
		return nil, err
	}

	if instanceTask == nil {
		return nil, nil
	}

	instanceState, err := readInstance(ctx, rb.rdb, instanceTask.ID)
	if err != nil {
		return nil, fmt.Errorf("reading workflow instance: %w", err)
	}

	// Read all pending events for this instance
	msgs, err := rb.rdb.XRange(ctx, pendingEventsKey(instanceTask.ID), "-", "+").Result()
	if err != nil {
		return nil, fmt.Errorf("reading event stream: %w", err)
	}

	newEvents := make([]*history.Event, 0, len(msgs))
	for _, msg := range msgs {
		var event *history.Event

		if err := json.Unmarshal([]byte(msg.Values["event"].(string)), &event); err != nil {
			return nil, fmt.Errorf("unmarshaling event: %w", err)
		}

		newEvents = append(newEvents, event)
	}

	return &task.Workflow{
		ID:                    instanceTask.TaskID,
		WorkflowInstance:      instanceState.Instance,
		WorkflowInstanceState: instanceState.State,
		Metadata:              instanceState.Metadata,
		LastSequenceID:        instanceState.LastSequenceID,
		NewEvents:             newEvents,
		CustomData:            msgs[len(msgs)-1].ID, // Id of last pending message in stream at this point
	}, nil
}

func (rb *redisBackend) ExtendWorkflowTask(ctx context.Context, taskID string, instance *core.WorkflowInstance) error {
	_, err := rb.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		return rb.workflowQueue.Extend(ctx, p, taskID)
	})

	return err
}

// Remove all pending events before (and including) a given message id
// KEYS[1] - pending events stream key
// ARGV[1] - message id
var removePendingEventsCmd = redis.NewScript(`
	local trimmed = redis.call("XTRIM", KEYS[1], "MINID", ARGV[1])
	local deleted = redis.call("XDEL", KEYS[1], ARGV[1])
	local removed =  trimmed + deleted
	return removed
`)

// KEYS[1] - pending events
// KEYS[2] - task queue stream
// KEYS[3] - task queue set
// ARGV[1] - Instance ID
var requeueInstanceCmd = redis.NewScript(`
	local pending_events = redis.call("XLEN", KEYS[1])
	if pending_events > 0 then
		local added = redis.call("SADD", KEYS[3], ARGV[1])
		if added == 1 then
			redis.call("XADD", KEYS[2], "*", "id", ARGV[1], "data", "")
		end
	end

	return true
`)

func (rb *redisBackend) CompleteWorkflowTask(
	ctx context.Context,
	task *task.Workflow,
	instance *core.WorkflowInstance,
	state core.WorkflowInstanceState,
	executedEvents, activityEvents, timerEvents []*history.Event,
	workflowEvents []history.WorkflowEvent,
) error {
	instanceState, err := readInstance(ctx, rb.rdb, instance.InstanceID)
	if err != nil {
		return err
	}

	// Check-point the workflow. We guarantee that no other worker is working on this workflow instance at this point via the
	// task queue, so we don't need to WATCH the keys, we just need to make sure all commands are executed atomically to prevent
	// a worker crashing in the middle of this execution.
	p := rb.rdb.TxPipeline()

	// Add executed events to the history
	if err := addEventsToHistoryStreamP(ctx, p, historyKey(instance.InstanceID), executedEvents); err != nil {
		return fmt.Errorf("serializing : %w", err)
	}

	for _, event := range executedEvents {
		switch event.Type {
		case history.EventType_TimerCanceled:
			removeFutureEventP(ctx, p, instance, event)
		}
	}

	// Schedule timers
	for _, timerEvent := range timerEvents {
		if err := addFutureEventP(ctx, p, instance, timerEvent); err != nil {
			return err
		}
	}

	// Send new workflow events to the respective streams
	groupedEvents := history.EventsByWorkflowInstanceID(workflowEvents)
	for targetInstanceID, events := range groupedEvents {
		// Insert pending events for target instance
		for _, m := range events {
			m := m

			if m.HistoryEvent.Type == history.EventType_WorkflowExecutionStarted {
				// Create new instance
				a := m.HistoryEvent.Attributes.(*history.ExecutionStartedAttributes)
				if err := createInstanceP(ctx, p, m.WorkflowInstance, a.Metadata, true); err != nil {
					return err
				}
			}

			// Add pending event to stream
			if err := addEventToStreamP(ctx, p, pendingEventsKey(targetInstanceID), m.HistoryEvent); err != nil {
				return err
			}
		}

		// Try to queue workflow task
		if targetInstanceID != instance.InstanceID {
			if err := rb.workflowQueue.Enqueue(ctx, p, targetInstanceID, nil); err != nil {
				return fmt.Errorf("enqueuing workflow task: %w", err)
			}
		}
	}

	instanceState.State = state

	if state == core.WorkflowInstanceStateFinished {
		t := time.Now()
		instanceState.CompletedAt = &t
	}

	if len(executedEvents) > 0 {
		instanceState.LastSequenceID = executedEvents[len(executedEvents)-1].SequenceID
	}

	if err := updateInstanceP(ctx, p, instance.InstanceID, instanceState); err != nil {
		return fmt.Errorf("updating workflow instance: %w", err)
	}

	// Store activity data
	for _, activityEvent := range activityEvents {
		if err := rb.activityQueue.Enqueue(ctx, p, activityEvent.ID, &activityData{
			Instance: instance,
			ID:       activityEvent.ID,
			Event:    activityEvent,
		}); err != nil {
			return fmt.Errorf("queueing activity task: %w", err)
		}
	}

	// Remove executed pending events
	if task.CustomData != nil {
		lastPendingEventMessageID := task.CustomData.(string)
		removePendingEventsCmd.Run(ctx, p, []string{pendingEventsKey(instance.InstanceID)}, lastPendingEventMessageID)
	}

	// Complete workflow task and unlock instance.
	completeCmd, err := rb.workflowQueue.Complete(ctx, p, task.ID)
	if err != nil {
		return fmt.Errorf("completing workflow task: %w", err)
	}

	// If there are pending events, queue the instance again
	keyInfo := rb.workflowQueue.Keys()
	requeueInstanceCmd.Run(ctx, p,
		[]string{pendingEventsKey(instance.InstanceID), keyInfo.StreamKey, keyInfo.SetKey},
		instance.InstanceID,
	)

	// Commit transaction
	executedCmds, err := p.Exec(ctx)
	if err != nil {
		if err := completeCmd.Err(); err != nil && err == redis.Nil {
			return fmt.Errorf("could not complete workflow task: %w", err)
		}

		for _, cmd := range executedCmds {
			if cmdErr := cmd.Err(); cmdErr != nil {
				rb.Logger().Debug("redis command error", "cmd", cmd.FullName(), "cmdErr", cmdErr.Error())
			}
		}

		return fmt.Errorf("completing workflow task: %w", err)
	}

	if state == core.WorkflowInstanceStateFinished {
		ctx = tracing.UnmarshalSpan(ctx, instanceState.Metadata)
		_, span := rb.Tracer().Start(ctx, "WorkflowComplete",
			trace.WithAttributes(
				attribute.String("workflow_instance_id", instanceState.Instance.InstanceID),
			))
		span.End()
	}

	return nil
}

func (rb *redisBackend) addWorkflowInstanceEventP(ctx context.Context, p redis.Pipeliner, instance *core.WorkflowInstance, event *history.Event) error {
	// Add event to pending events for instance
	if err := addEventToStreamP(ctx, p, pendingEventsKey(instance.InstanceID), event); err != nil {
		return err
	}

	// Queue workflow task
	if err := rb.workflowQueue.Enqueue(ctx, p, instance.InstanceID, nil); err != nil {
		return fmt.Errorf("queueing workflow: %w", err)
	}

	return nil
}
