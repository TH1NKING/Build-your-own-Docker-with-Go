# Bind a System Call Policy to each Runtime Profile

Every Runtime Profile owns an operator-reviewed seccomp allowlist, and production denies any system call or argument pattern not explicitly required by that profile; Workloads and models cannot override it. The Runtime Lab may audit representative Workloads to discover missing calls, but additions require review, profile tests, and a new Runtime Profile version rather than silently broadening an existing profile, with parameter checks used where calls such as thread creation have both required and namespace-creating forms.
