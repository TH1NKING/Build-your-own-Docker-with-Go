---
status: superseded by ADR-0025
---

# Limit the first release to private deployments

The first release trusts the Platform Operator but treats Users, user data, and all LLM-generated Workloads as untrusted, with isolation required between Users. It targets self-hosted private deployments and does not claim to safely execute arbitrary code submitted by anonymous Internet attackers, keeping the initial security model achievable without weakening the isolation requirements inside that boundary.
