# Bound Model Provider retries

For each model decision, the Control Plane permits the initial Model Provider request plus at most two automatic retries, and caps the whole Agent Run at twelve model request attempts. It retries only transport interruptions, timeouts, HTTP 429 responses while honoring `Retry-After`, and HTTP 5xx responses; authentication and other non-retriable 4xx responses, malformed compatible-protocol responses, and invalid Tool Calls fail the Agent Run, after which the Platform Operator must explicitly start a replacement Run.

Streamed deltas remain provisional until one complete, valid response has been received, so a partial stream can be abandoned without persisting a Message or executing an incomplete Tool Call. Once a complete model response is durably recorded it is never requested again automatically, preventing a recovery path from duplicating a model decision or Workload Execution.
