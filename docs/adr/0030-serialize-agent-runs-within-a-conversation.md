# Serialize Agent Runs within a Conversation

The first release permits at most one nonterminal Agent Run in each Conversation: while it is active, the Operator Console rejects rather than queues or injects another Message, and the Platform Operator may cancel the active Run before submitting a replacement request. Different Conversations may run concurrently subject to Worker Node capacity and the Execution queue, preserving deterministic message order, Model Context, Conversation Summary updates, and Sandbox ownership without imposing a global single-Run restriction on the installation.
