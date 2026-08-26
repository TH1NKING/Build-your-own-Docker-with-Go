---
status: superseded by ADR-0025
---

# Use Users as the first-release ownership boundary

The first release models each natural person as one User and makes that User the direct owner of Agent Runs, data, history, quotas, and access decisions instead of introducing a Tenant or organization layer. This keeps the initial authorization model small; ownership checks must remain centralized so a future release can add personal or organizational Tenants, backfill one personal Tenant per existing User, and migrate owned resources without rewriting every call site.
