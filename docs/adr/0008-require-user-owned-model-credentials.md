---
status: superseded by ADR-0025
---

# Require User-owned Model Credentials

The first release uses BYOK: every User selects an operator-approved Model Provider and supplies a personal Model Credential, while the platform provides no shared model credential and runs no local model. The Control Plane encrypts credentials at rest with a separately managed master key and invokes providers on the User's behalf; credentials are never returned, logged, included in Model Context, or sent to Worker Nodes or Sandboxes, and User-supplied provider URLs are rejected to avoid turning the Control Plane into an SSRF primitive.
