# Ship one operator-configured model adapter

The Control Plane defines an internal Model Provider Adapter boundary but the first release ships only an `openai-compatible-v1` adapter and permits one active, installation-wide Model Provider configuration containing `base_url`, `model`, and Model Credential. Compatibility means only the request, response, and Tool Call subset explicitly documented and tested by this project; the first release provides no local model, provider catalog, per-Conversation model selection, multiple credentials, configuration UI, or hot switching, keeping the system an agent runtime rather than a model gateway.

Because the Platform Operator is trusted, the configured `base_url` is operator-controlled startup configuration rather than untrusted runtime input. The Model Credential remains inside the Control Plane and is never sent to Worker Nodes or Sandboxes.
