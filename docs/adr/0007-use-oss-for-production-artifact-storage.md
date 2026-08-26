# Use OSS for production Artifact storage

Development uses a local filesystem ArtifactStore, while production uses a private, pay-as-you-go Alibaba Cloud OSS bucket with quotas, lifecycle rules, and cost alerts; no large capacity plan is purchased initially. The Control Plane's 40 GiB system disk stores only the database, metadata, and bounded logs, keeping operator files and Run output growth from exhausting the operating-system volume while preserving a simple local development path.
