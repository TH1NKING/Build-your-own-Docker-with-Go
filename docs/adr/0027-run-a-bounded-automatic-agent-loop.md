# Run a bounded automatic Agent Loop

After the Model Provider returns a valid `execute_python` Tool Call, the Control Plane durably records it, dispatches its Execution to the self-developed Sandbox, records the Execution Result, and automatically invokes the model again with that result; this serial Agent Loop continues without per-Execution operator approval until the model returns a final answer. A failed Workload Execution is returned to the model for possible correction, while the Run terminates on operator cancellation, an irrecoverable model, Worker, or protocol failure, an exhausted Resource Budget, or the first-release maximum of eight Tool Calls.

The first release accepts at most one Tool Call from each model decision and does not execute parallel tool calls. Together with durable state transitions and ADR-0009, this ensures a Worker-capacity wait resumes the already-recorded Tool Call rather than repeating the preceding model request or charging for a duplicate decision.
