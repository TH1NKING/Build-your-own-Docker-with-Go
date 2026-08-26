# Assign one Sandbox to each Agent Run

Each Agent Run receives an exclusive Sandbox that may be reused by its sequential Executions, but a Sandbox is never shared across Agent Runs and is destroyed when its Agent Run completes, expires, or is cancelled. This preserves files and dependencies across model-driven retries while preventing state leakage between independent tasks, at the cost of requiring per-Execution process cleanup and cumulative Run-level resource limits.
