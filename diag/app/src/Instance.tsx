import { Accordion, Alert, Card } from "react-bootstrap";
import { Link, useParams } from "react-router-dom";
import {
  ExecutionCompletedAttributes,
  ExecutionStartedAttributes,
  HistoryEvent,
  WorkflowInstanceInfo,
} from "./client";
import {
  decodePayload,
  decodePayloads,
  EventType,
  Payload,
  ScheduleEventID,
  WorkflowInstanceState,
} from "./Components";

import useFetch from "react-fetch-hook";
import { InstanceTree } from "./InstanceTree";

function Instance() {
  let params = useParams();

  const instanceId = params.instanceId;

  const {
    isLoading,
    data: instance,
    error,
  } = useFetch<WorkflowInstanceInfo>(
    document.location.pathname + "api/" + instanceId
  );

  if (isLoading) {
    return <div>Loading...</div>;
  }

  if (error || !instance) {
    return (
      <div>
        <Alert variant="danger">
          Workflow instance with id <code>{instanceId}</code> not found
        </Alert>
      </div>
    );
  }

  const startedEvent = instance.history.find(
    (e) => e.type === "WorkflowExecutionStarted"
  ) as HistoryEvent<ExecutionStartedAttributes>;

  const workflowName = startedEvent.attributes.name;
  const inputs = startedEvent.attributes.inputs;

  let wfResult: string | undefined;
  let wfError: string | undefined;
  const finishedEvent = instance.history.find(
    (e) => e.type === "WorkflowExecutionFinished"
  ) as HistoryEvent<ExecutionCompletedAttributes>;
  if (finishedEvent) {
    wfResult = finishedEvent.attributes.result;
    wfError = finishedEvent.attributes.error;
  }

  return (
    <div>
      <div className="d-flex align-items-center">
        <h2>
          Workflow: <code>{workflowName}</code>
        </h2>
      </div>

      <dl className="row">
        <dt className="col-sm-4">InstanceID</dt>
        <dd className="col-sm-8">
          <code>{instance.instance.instance_id}</code>
        </dd>

        <dt className="col-sm-4">ExecutionID</dt>
        <dd className="col-sm-8">
          <code>{instance.instance.execution_id}</code>
        </dd>

        {!!instance.instance.parent_instance && (
          <>
            <dt className="col-sm-4">Parent InstanceID</dt>
            <dd className="col-sm-8">
              <Link to={`/${instance.instance.parent_instance}`}>
                {instance.instance.parent_instance}
              </Link>
            </dd>
          </>
        )}

        <dt className="col-sm-4">State</dt>
        <dd className="col-sm-8">
          <WorkflowInstanceState state={instance.state} />
        </dd>

        <dt className="col-sm-4">Created at</dt>
        <dd className="col-sm-8">{instance.created_at}</dd>

        <dt className="col-sm-4">Completed at</dt>
        <dd className="col-sm-8">
          {!instance.completed_at ? <i>pending</i> : instance.completed_at}
        </dd>
      </dl>

      <Card>
        <Card.Header as="h5">Input</Card.Header>
        <Card.Body>
          {inputs && (
            <Payload payloads={inputs?.map((i) => decodePayload(i))} />
          )}
        </Card.Body>
      </Card>

      <Card className="mt-3">
        <Card.Header as="h5">Result</Card.Header>
        <Card.Body>
          {wfResult && <Payload payloads={[decodePayload(wfResult)]} />}
          {wfError && <Payload payloads={[wfError]} />}
        </Card.Body>
      </Card>

      <h2 className="mt-4">Workflow Graph</h2>
      <InstanceTree instanceId={instance.instance.instance_id} />

      <h2 className="mt-3">History</h2>
      <Accordion alwaysOpen>
        {instance.history.map((event, idx) => (
          <Accordion.Item eventKey={`${idx}`} key={event.id}>
            <Accordion.Header>
              <h5 className="d-flex flex-grow-1 align-items-center pe-3">
                <div className="text-secondary" style={{ width: "50px" }}>
                  #{event.sequence_id}
                </div>
                <div className="flex-grow-1">
                  <EventType type={event.type} />

                  {!!event.schedule_event_id && (
                    <ScheduleEventID id={event.schedule_event_id!} />
                  )}
                </div>
                {event.type !== "WorkflowExecutionStarted" && <div className="flex-grow-1"><code>{event.attributes?.name}</code></div>}
                <div>{event.timestamp}</div>
              </h5>
            </Accordion.Header>
            <Accordion.Body>
              <dl>
                <dt>Event ID</dt>
                <dd>{event.id}</dd>
                <dt>Schedule Event ID</dt>
                <dd>
                  {!event.schedule_event_id ? (
                    <i>none</i>
                  ) : (
                    event.schedule_event_id
                  )}
                </dd>
                {event.visible_at && (
                  <>
                    <dt>Visible At</dt>
                    <dd>{event.visible_at}</dd>
                  </>
                )}
                <dt>Attributes</dt>
                <dd>
                  <Payload
                    payloads={[
                      JSON.stringify(
                        decodePayloads(event.attributes),
                        undefined,
                        2
                      ),
                    ]}
                  />
                </dd>
              </dl>
            </Accordion.Body>
          </Accordion.Item>
        ))}
      </Accordion>
    </div>
  );
}

export default Instance;
