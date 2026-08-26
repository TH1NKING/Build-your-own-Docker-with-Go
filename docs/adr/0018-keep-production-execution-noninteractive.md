# Keep production Execution noninteractive

The first release exposes no Terminal, SSH, WebSocket shell, or attach operation for production Sandboxes: the Platform Operator interacts through Conversations, models request bounded Executions through structured Tool Calls, and the system returns immutable Execution Results. PTYs, `devpts`, interactive shells, reconnect semantics, and direct operator-to-Sandbox streams remain exclusive to the local Runtime Lab, keeping the production product focused on AI Agent execution rather than becoming a cloud IDE.
