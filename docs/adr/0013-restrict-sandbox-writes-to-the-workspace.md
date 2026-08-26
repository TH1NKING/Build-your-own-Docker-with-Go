# Restrict Sandbox writes to the Workspace

Production Sandboxes use a read-only root filesystem and expose Attachments under read-only `/workspace/input`; only quota-bound `/workspace/output` and ephemeral `/tmp` are writable. Executions in the same Agent Run share output state, but after the Run ends only explicitly selected files are promoted to Run Artifacts and all remaining Workspace and temporary state is deleted, preventing Workloads from modifying inputs or the operator-managed Runtime Profile while preserving multi-step Agent workflows.
