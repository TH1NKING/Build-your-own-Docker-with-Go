# Separate the Control Plane and Worker Nodes in production

Production deployments place the trusted Control Plane and untrusted Workloads on separate operating-system instances, and Sandboxes run only on dedicated Worker Nodes. Development may use an all-in-one single-machine profile for convenience, but process separation alone is not treated as a security boundary; this keeps a successful Sandbox escape away from control-plane credentials and durable platform data.
