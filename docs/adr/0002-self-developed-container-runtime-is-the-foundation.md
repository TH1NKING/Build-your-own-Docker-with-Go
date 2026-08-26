# Keep the self-developed container runtime as the execution foundation

The platform will evolve the repository's self-developed Linux container runtime into the foundation of Sandbox execution instead of replacing it with Docker, gVisor, Kata Containers, or a microVM runtime. This preserves the project's purpose and makes isolation behavior inspectable, accepting that the first release must have a narrower threat model while the native runtime is hardened incrementally.
