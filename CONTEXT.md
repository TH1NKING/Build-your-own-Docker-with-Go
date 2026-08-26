# Self-Hosted Agent Sandbox

The system provides isolated execution environments for code produced or selected by AI agents. Its first release is a single-operator, self-hosted tool: the Platform Operator is trusted, while Workloads remain untrusted.

## Language

**Platform Operator**:
The trusted person who deploys, configures, administers, and uses one private installation and owns all of its Conversations and Agent Runs.
_Avoid_: User, Tenant, customer, account

**Agent Run**:
A bounded, durable attempt by an AI agent to complete one Platform Operator request, potentially involving multiple model decisions, Workload executions, and pauses while required capacity is unavailable.
_Avoid_: Session, conversation, job

**Agent Loop**:
The bounded progression within one Agent Run that alternates model decisions, Tool Calls, and Execution Results until a final answer or terminal condition is reached.
_Avoid_: Chain, workflow, tool loop

**Agent Run Event**:
An ordered, resumable observation of an Agent Run state change intended for the Operator Console without exposing internal database state.
_Avoid_: Log line, notification, SSE message

**Conversation**:
A durable sequence of Messages and Agent Runs that preserves the history of one continuing interaction within an installation.
_Avoid_: Agent Run, session, thread

**Conversation Summary**:
A compact, replaceable representation of older Conversation history used to assemble Model Context without replacing the durable source Messages.
_Avoid_: Memory, RAG index, Conversation

**Attachment**:
A Platform Operator-provided or explicitly retained file that belongs to a Conversation and shares its lifetime.
_Avoid_: Upload, blob, Run Artifact

**Run Artifact**:
A file explicitly declared as output by a Tool Call, safely extracted from an Agent Run's Workspace, and retained temporarily unless the Platform Operator attaches it to a Conversation.
_Avoid_: Attachment, output file, result

**Sandbox File**:
A temporary file that exists only inside a Sandbox and is deleted with that Sandbox unless promoted to a Run Artifact.
_Avoid_: Attachment, Run Artifact

**Workspace**:
The filesystem area assigned exclusively to one Agent Run, containing immutable inputs and quota-bound writable output shared by that Run's Executions.
_Avoid_: Working directory, volume, rootfs

**Resource Budget**:
The cumulative CPU, memory, process, time, storage, file-count, and output allowance granted to one Sandbox and its Agent Run.
_Avoid_: Limit, quota, cgroup

**Model Context**:
The bounded selection of Conversation history, summaries, Attachments, and current input supplied to the model for one decision. It is not the complete stored Conversation.
_Avoid_: Conversation, chat history, prompt

**Tool Call**:
A durable request produced by a Model Provider for the platform to perform one supported action as part of an Agent Run.
_Avoid_: Command, function, Execution

**Model Provider**:
A Platform Operator-approved external service that supplies model responses for Agent Runs using the installation's configured credential.
_Avoid_: LLM endpoint, AI vendor, custom URL

**Model Provider Adapter**:
The Control Plane boundary that translates Agent Run model decisions and Tool Calls to and from one supported Model Provider protocol.
_Avoid_: LLM wrapper, SDK, model gateway

**Model Credential**:
A secret configured by the Platform Operator that authorizes one installation's Control Plane to call a Model Provider.
_Avoid_: API Key, token, platform key

**Workload**:
Untrusted executable code and its declared inputs produced or selected during an Agent Run.
_Avoid_: Script, command, container

**Sandbox**:
The isolated execution environment assigned exclusively to one Agent Run, with an explicit lifetime and resource boundary.
_Avoid_: Docker, virtual machine, runner

**Network Policy**:
The operator-controlled rule determining whether a Sandbox can exchange network traffic; the first-release production policy is always `none`.
_Avoid_: Firewall, network mode, runtime setting

**Runtime Profile**:
An operator-managed, versioned execution environment defining the language runtime and installed dependencies available to a Workload. The first release exposes only `python-data-v1` while the underlying container runtime remains language-neutral.
_Avoid_: Image, rootfs, language, environment

**Profile Bundle**:
An immutable, digest-identified package containing one Runtime Profile's root filesystem, locked dependencies, Sandbox Init, manifest, and System Call Policy.
_Avoid_: Docker image, tarball, rootfs

**System Call Policy**:
The operator-maintained allowlist of kernel operations available to Workloads in one Runtime Profile, enforced in production and never relaxed by Workloads or models.
_Avoid_: seccomp profile, blacklist, syscall filter

**Runtime Lab**:
The explicitly non-production environment that exposes lower-level container mechanisms for learning, comparison, and integration testing without inheriting the production Sandbox security claim.
_Avoid_: Development mode, production Sandbox, debug mode

**Execution**:
One bounded attempt to run a Workload inside an Agent Run's Sandbox. An Agent Run may contain multiple sequential Executions that share Sandbox state.
_Avoid_: Job, process, attempt

**Execution Result**:
The bounded, immutable outcome of one Execution, including its terminal status, captured output, resource usage, and references to any promoted Run Artifacts.
_Avoid_: Terminal output, response, log

**Execution Lease**:
A time-limited, generation-stamped grant giving one Worker Node exclusive authority to perform and report one Execution.
_Avoid_: Lock, assignment, heartbeat

**Control Plane**:
The trusted system role that accepts Platform Operator requests, owns durable state, and coordinates Agent Runs without executing Workloads.
_Avoid_: Server, master, backend

**Operator Console**:
The local-facing interface through which the Platform Operator manages Conversations, Agent Runs, Attachments, Run Artifacts, and Worker Node status.
_Avoid_: Public UI, admin panel, dashboard

**Worker Node**:
An execution-plane machine that accepts assigned Agent Runs and hosts their Sandboxes without owning durable platform state.
_Avoid_: Agent, executor, slave

**Worker Credential**:
A revocable secret identifying and authorizing exactly one Worker Node to use its scoped Control Plane operations.
_Avoid_: User token, API key, shared secret

**Sandbox Supervisor**:
The minimal privileged local component that creates, enters, terminates, and cleans Sandboxes on behalf of an otherwise unprivileged Worker Node.
_Avoid_: Worker, root daemon, helper

**Sandbox Init**:
The trusted PID 1 inside one Sandbox that keeps it alive across sequential Executions, reaps processes, and reports Execution results through a private inherited control channel.
_Avoid_: Workload, shell, daemon, supervisor
