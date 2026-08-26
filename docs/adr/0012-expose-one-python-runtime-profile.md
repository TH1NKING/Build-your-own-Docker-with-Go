# Expose one Python Runtime Profile initially

The Control Plane, Worker Node, Agent orchestration, and self-developed container runtime are implemented in Go, and the container runtime remains capable of launching arbitrary Linux processes. The first-release Agent product exposes only an operator-managed `python-data-v1` Runtime Profile with fixed dependencies; Shell remains an internal runtime mechanism, while Go source compilation and arbitrary binaries are deferred to keep the untrusted Workload surface and test matrix small without turning the project itself into a Python implementation.
