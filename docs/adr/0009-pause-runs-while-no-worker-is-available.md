# Pause Agent Runs while no Worker Node is available

When a model produces a Tool Call and no Worker Node is available, the Agent Run enters a durable waiting state instead of failing or invoking the model again. The Control Plane persists the Tool Call, allows the Platform Operator to cancel while waiting, and resumes with the stored request when a Worker Node becomes available, so temporary execution-plane unavailability neither loses progress nor duplicates model cost.
