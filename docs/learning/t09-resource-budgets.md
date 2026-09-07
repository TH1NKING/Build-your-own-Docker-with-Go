# T09：给 Sandbox 加上 CPU、内存、swap 和 PID 资源预算

T05 解决了“在同一个 Sandbox 中连续执行 Python”，T06 解决了“结束时可靠地清理资源”。T09 接着解决资源失控：即使 Workload 没有突破 namespace，它仍可能持续计算、分配内存，或者不断创建后代进程。现在 Supervisor 为 Sandbox 设置可信 Resource Budget，在不可信代码开始运行前建立约束，并将资源事件与 Execution Result 联系起来。

这个变化的价值不只是给几个 cgroup 文件写入数字。更难的部分是把预算放在正确的生命周期上，保证启动顺序没有空档，区分不同资源的超限行为，并在清理失败时保留继续处理的能力。读完本文，你应该能解释这四件事，也能沿着代码验证它们。

本文对应 [T09 / #15](https://github.com/TH1NKING/Build-your-own-Docker-with-Go/issues/15)，沿用 [ADR-0014 的初始预算](../adr/0014-start-with-conservative-sandbox-resource-budgets.md) 与 [ADR-0016 的常驻 Init](../adr/0016-keep-one-sandbox-init-per-agent-run.md)。T07、T08、T10、T11、T12 的后续合同仍需完成；本次结果不能作为“已经能够在生产环境安全执行任意不可信代码”的证明。

## 先把“隔离”和“预算”分清楚

可以把 namespace 理解成限制 Workload 看见和使用哪一套进程编号、挂载点及身份映射；Resource Budget 则回答，在获准使用的资源中，它最多能消耗多少。进程看不见宿主其他进程，并不意味着它不能把宿主 CPU 跑满。

本次由 Linux cgroup v2 承担资源约束。Supervisor 在宿主侧设置策略，Workload 和普通 Worker 请求不能自行提高预算。领域概念仍叫 Resource Budget，cgroup 是实现这个概念所选用的内核机制。

默认配置如下；具体配置字段与校验入口见 [resource_budget_linux.go](../../internal/sandboxsupervisor/resource_budget_linux.go)，启动参数见 [sandboxd](../../cmd/sandboxd/main_linux.go)。

| 资源 | 默认值 | 应如何理解 |
| --- | --- | --- |
| CPU | 2000 millicores | 相当于 2 个逻辑 CPU 的执行时间配额；不会固定绑定到某两个 CPU |
| 内存 | 1 GiB | Sandbox 子树的内存预算，Init 与仍被计费的 Workspace 内容也在其中 |
| swap | 0 | 不允许这个 Sandbox 通过 swap 扩展可用交换空间 |
| PID | 64 | 限制内核任务数量；线程也占名额，不等于允许 64 个 Python 程序 |

这些值由可信 Platform Operator 在 Supervisor 启动时配置。设置在请求外面，能避免 Workload 或模型通过一次 Tool Call 给自己选择无限预算。代价是同一 Supervisor 的配置比较统一；将来如需按 Runtime Profile 或 Agent Run 分档，需要增加经过授权的策略选择合同。

内存与 swap 使用页大小对齐的字节数，CPU 使用固定周期换算配额，PID 必须为正数。提前拒绝无效配置，是为了让实际内核配置与平台对调用方的承诺一致。不能把配置解析成功当成约束已经生效，还要检查控制器、写入是否成功以及内核读回的值。

## 为什么采用两层 cgroup

下面用概念名称表示结构；实际目录名由 Supervisor 生成，不使用客户端提供的字符串拼接路径。

```text
Operator 委派给 Supervisor 的 cgroup v2 目录
└── Sandbox 预算父组：与整个 Sandbox 同寿命
    ├── Init 叶子组：可信 bootstrap → 常驻 Sandbox Init
    └── Execution 叶子组：可信 launcher → Python 及所有后代
```

一次 Execution 清理后，它的叶子组会被删除；下一次 Execution 创建新的叶子组。Init 与预算父组继续存在，Workspace 文件也可以继续使用。整个 Sandbox 销毁时，才清理完整的树。

第一层解决“总账放在哪里”。假设第一次 Execution 向 tmpfs Workspace 写入文件，退出后第二次还要读取。这些文件占用的内存不会因为 Python 退出就消失。因此，只给每次 Execution 一个独立预算，然后删掉旧组，不能表达整个 Sandbox 尚在使用的资源。保留共同父组，让旧文件占用与后续执行继续受同一个内存预算约束。

这里不要把“删除 cgroup 目录”理解成“释放这个组曾经分配过的每一页内存”。内存计费与进程的当前成员关系不是同一件事；迁移进程也不能简单地搬走它之前产生的所有内存计费。本实现因此让 bootstrap 从创建时就进入预算子树，而不等它初始化完再迁移。[Linux cgroup v2 文档](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory-ownership)

第二层解决“杀谁”。如果 Init 与 Python 在同一个 Execution 组里，向 `cgroup.kill` 写入 `1` 就会连同 Init 一起终止。后续 Execution 需要的常驻 PID 1 也消失了。把它们放在同一预算父组的不同叶子中，能够单独终止某次 Execution 的全部进程。

预算父组本身不承载进程，这也符合 cgroup v2 分配这些 domain controller 时对内部节点的要求。树形结构在这里同时表达了两个边界：父组是共享资源边界，Execution 叶子是单次终止边界。[Linux 的 no internal process 约束](https://docs.kernel.org/admin-guide/cgroup-v2.html#no-internal-process-constraint)

| 方案 | 好处 | 放到本项目中会付出的代价 |
| --- | --- | --- |
| 每次 Execution 单独一个组，没有 Sandbox 父预算 | 单次执行的目录与计数直观 | 难以表达跨 Execution 保留的 Workspace 内存与 Init 的共同预算 |
| Init 与 Workload 放进同一个叶子 | 树最简单，统一计费 | 整组终止会杀掉 PID 1，使同一 Sandbox 无法继续执行 |
| 每次执行都销毁并重建 Sandbox | 每次都从干净环境开始，生命周期简单 | Workspace 延续需要额外保存、搬运和恢复，并增加初始化成本 |
| 本次采用父预算加两个叶子 | 同时保留 Sandbox 总预算、单次清理与状态延续 | 创建、事件归因与按层清理都需要显式管理 |

## CPU 超限为什么通常不是失败

默认的 2000 millicores 换算到 100 毫秒周期，是 `cpu.max = 200000 100000`。这里两个数字都使用微秒，表示每个周期可消耗的总 CPU 时间及周期长度。因为多个任务可以在多个 CPU 上同时运行，配额可以大于一个周期的墙钟时间。

举例说，4 个可运行任务在 4 个 CPU 上各运行约 50 毫秒，就可能合计消耗 200 毫秒 CPU 时间。配额用完后它们会被节流，等之后的周期再继续。因此，多进程一起执行也不能把每个进程各当成一份独立的 2 CPU 配额。[Linux CPU controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#cpu)

这是配额语义的示意，不是本项目测得的时序或性能保证。真实调度还受宿主竞争、上层 cgroup 约束、硬件和内核实现影响。

CPU 配额控制的是消耗速率，并不限制程序总共跑多久。一个被节流的无限循环仍然可以一直运行，所以资源结果不能把“出现 CPU 节流”自动翻译成“Execution 失败”。执行超时及 Agent Run 累计时间是另外的合同，属于后续 T12 等工作。

`cpu.weight` 更适合在竞争时按比例分配 CPU，没有竞争时可以使用更多资源；它不能直接表达本次要求的固定上界。CPU affinity 则适合限定在哪些 CPU 上运行，也不是同一个维度。这里选择 `cpu.max`，是因为需要限制 Sandbox 合计消耗的 CPU 时间。

## 内存与 swap 为什么要一起配置

Supervisor 设置内存预算，也显式写入 swap 预算。默认 swap 为零，让配置含义明确；如果允许 swap，需要由可信启动配置给出有限值。swap 的预算在 cgroup v2 中有独立接口，因此不能只设置 `memory.max` 就把“禁用 swap”当成已经实现。[Linux memory controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory)

内存压力与 CPU 不一样。达到内存上界后，内核可能先回收；无法满足分配时可能出现 cgroup OOM。平台观察内核资源事件，并终止本次 Execution 的剩余后代，再把资源原因写入结果。

`memory.oom.group=1` 设置在 Execution 叶子上，目的是让这个 Workload 作为一个整体处理，而不让其中一个进程被杀后其他进程继续运行在残缺状态。Supervisor 仍然执行自己的整组终止、等待和回收流程：内核 OOM 策略负责内存压力下的选择，平台负责完成单次执行的结束合同。

单看退出码 `137` 不能证明发生过 OOM。它通常表达被 `SIGKILL` 终止，但同样的信号也可能来自显式销毁或其他路径。因此，本实现依赖宿主读取的 cgroup 事件证据，而不是从 Python 的异常文字或退出码猜原因。

## 为什么保护 Init，又要撤销 Workload 继承的保护

Init 是执行协议和进程回收的可信参与者。如果内存紧张时先杀掉它，原 Sandbox 就失去继续执行的条件。因此，宿主 Supervisor 为 Init 设置 `oom_score_adj=-1000`，让 OOM killer 的常规选择避开这个进程。

这个设置会由子进程继承。若直接让 Python 保留相同值，原本为可信管理进程安排的保护就会落到不可信 Workload 身上。实现因此在 launcher 仍然阻塞时，从宿主把它设回 `0`，确认成功后再允许执行 Python。[oom_score_adj 的语义与继承规则](https://man7.org/linux/man-pages/man5/proc_pid_oom_score_adj.5.html)

这不是 Init 的存活保证。它仍然处在 Sandbox 总内存预算内，分配失败可能破坏协议运行；进程也可能因其他错误或外部终止而退出。一旦 Init 丢失，已有生命周期合同会使 Sandbox 失效，不能假装它仍可接收下一次 Execution。

相较于直接把 Init 放在 Sandbox 预算外，本次做法能把其开销纳入 Sandbox 总账；代价是必须处理管理进程也遭受内存压力的情况。即使成功返回资源超限原因，是否能复用该 Sandbox 仍取决于 Init 与清理握手是否正常完成。

## PID 上限为什么需要平台补一个动作

PID controller 限制的是任务数量，包含线程。达到上限时，内核主要阻止新增任务；它不会因为某次 `fork` 失败就替平台终止已经存在的全部 Python 进程。[Linux PID controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#pid)

一个 Workload 可以捕获创建失败，转而继续计算，或者让已经启动的后代继续运行。本项目把观察到 PID 上限事件定义为本次 Execution 超出 Resource Budget，所以 Supervisor 再执行 `cgroup.kill`，等待后代退出，并报告对应资源原因。

这是“内核约束”和“平台策略”的区别。内核负责立即拒绝超过 PID 预算的新任务；Supervisor 负责观察事件后结束这次执行。即使事件轮询有延迟，创建上界也不依赖轮询速度，但从事件出现到整次 Execution 被终止仍存在检测与调度延迟。

如果只采用 `RLIMIT_NPROC`，需要考虑它按身份计数及相应权限语义，较难直接表达这里的 Sandbox 子树边界。只追踪最初的 Python PID，则会遗漏后代和线程。cgroup 的进程成员关系与现有整组清理机制能够衔接，适合本项目的执行模型。

## 启动顺序为什么不能交换

整个顺序可以分成“允许可信初始化”和“允许不可信代码执行”两个阶段：

1. Supervisor 创建预算父组与 Init 叶子，配置并检查资源约束。
2. bootstrap 创建时直接进入 Init 叶子，再完成隔离初始化并成为常驻 Init。
3. 开始 Execution 时创建独立叶子，配置 OOM 分组并读取事件基线。
4. Init 启动可信 launcher；launcher 等待 gate，尚未执行 Python。
5. Supervisor 确认 launcher 的宿主 PID、迁移到 Execution 叶子、撤销继承的 OOM 保护。
6. 前述操作均成功后才发送 `run`，打开 gate，执行 Python。

如果先启动 Python 再迁移，Python 可能已经产生后代，迁移原进程不会自动把这些既有后代一起搬走。gate 把“可能赶得上”改成了可检查的顺序关系。bootstrap 直接进入 Init 叶子，则解决可信启动本身的计费位置。

不要因此声称 launcher 从诞生开始的每一点开销都计在 Execution 叶子：它最初在 Init 叶子中运行，之后才被迁移；两个叶子都受同一个 Sandbox 父预算约束。这个区分能解释为什么“预算覆盖整个 Sandbox”与“每次 Execution 的独立统计”不是完全相同的承诺。

该流程要求实际可用的 cgroup v2、所需控制器和内核接口。缺失控制器、配置无法确认、归组失败或 OOM 调整失败时，创建或执行失败，Workload 不会被放行。当前没有自动退回 cgroup v1，也没有静默取消部分资源预算的路径。

这次实现还遇到了一个值得记住的细节：不能用“向空组写入 `cgroup.kill=1`”代替检查清理接口是否可用。在本次 Linux 6.12.107 验证中，这会改变组的 fork/kill 序号，使之后通过 `CLONE_INTO_CGROUP` 出生的 Init 在内核 `cgroup_post_fork` 阶段收到终止信号。最终实现只打开该接口确认可写，等真正清理时才发出终止指令。这个问题说明，带副作用的接口不能随意当成能力探针；创建成功的真实内核测试也有编译测试无法替代的价值。

## 为什么比较事件的增量

Sandbox 父组跨 Execution 存活，所以它的资源事件计数也会累积。第一次发生过 OOM 后，第二次正常执行不能因为读到同一个非零计数就再次被标记为内存超限。

因此，在一次 Execution 开始前记录基线，运行中观察变化，结束时再次读取并计算差值。顺序 Execution 的现有约束使这条归因路径更清楚：同一个 Sandbox 不会同时运行两个 Workload，让两个请求争相解释同一次计数增长。

计数不是“超限字节数”，也不是精确的内存峰值；OOM 事件次数也不能直接当成被杀进程数。结果中每个资源字段都必须保留它原本的统计含义。CPU 节流统计用于说明执行受到了配额约束，OOM 与 PID 事件用于支持相应的结束原因，不能统一包装成一个含义不明的“资源超限”。

实际返回的 `terminal_reason` 为 `exited`、`memory_limit` 或 `pids_limit`。`resource_usage` 包含 `cpu_usec`、`cpu_throttled_periods`、`cpu_throttled_usec`、`oom_events` 和 `pid_limit_events`，均是本次观察区间的增量。如果 OOM 与 PID 事件同时出现，主原因选择 `memory_limit`，同时保留两个事件增量。普通非零退出仍属于 `exited`，不能仅凭退出失败推断超限。

若已经取得明确的内核超限证据，但 Init 随后失去响应，结果仍保留资源原因；无法确认的退出码用 `-1`，输出为空并标记 `truncated=true`，而不是编造一次完整的进程结果。此时 Sandbox 会失效并清理。完整字段语义见 [协议文档](../sandbox-supervisor-protocol-v1.md)。

读资源证据本身失败，也意味着平台无法完成原先承诺的观察与分类。此时不能凭缺失数据报告一次普通成功，应该进入失败收尾。事件监控与 Init 响应是并发的，但结束时还要复读证据，处理资源事件与进程退出几乎同时发生的情况。

## 为什么 kill 成功后还不能立刻返回

结束 Execution 至少需要区分以下三件事：内核已收到终止请求，组里已没有活进程，Init 已回收退出状态并完成输出收集。`cgroup.kill`、`cgroup.events` 的 `populated 0`、Init 的 `reap → ready` 握手分别提供这些阶段所需的动作或证据。

保留这个顺序，才能避免后台后代继续写 Workspace，或仍持有输出管道导致后续读取挂住。资源预算没有替代 T05 的后代回收与 T06 的 Sandbox 生命周期；它在相同路径上增加了新的终止触发条件。

清理从 Execution 叶子开始，整个 Sandbox 结束时再清理 Init 叶子和预算父组。如果某一步失败，Supervisor 保留自己持有的 cgroup 目录句柄、Sandbox 记录及身份预留，以便后续销毁请求重试。清理成功以后才能释放身份，让其他 Sandbox 使用。

只记录一个路径字符串然后忘掉旧资源，看起来能让错误路径更短，却可能留下仍占用资源的内核对象；马上复用身份还会让新旧资源混在一起。保留所有权会暂时占住容量，这是一个明确的取舍：不能确认旧资源已经释放时，先保留继续收尾的能力。

这里的重试仍属于当前 Supervisor 进程的生命周期。它不包含重启后恢复 Sandbox、扫描所有孤儿资源或持久保存清理状态。

## 建议按这个顺序读代码

| 顺序 | 文件 | 带着什么问题阅读 |
| --- | --- | --- |
| 1 | [资源配置](../../internal/sandboxsupervisor/resource_budget_linux.go) | 默认值是什么，哪些配置必须在服务启动前拒绝？ |
| 2 | [cgroup 管理](../../internal/sandboxsupervisor/cgroup_linux.go) | 父子树由谁创建，配置在哪一层，失败后谁还持有清理能力？ |
| 3 | [Sandbox 创建](../../internal/sandboxsupervisor/creation_linux.go) | Init 怎样从创建时就进入预算，OOM 保护在哪一步设置？ |
| 4 | [Execution 流程](../../internal/sandboxsupervisor/execution_linux.go) | gate 何时打开，事件基线何时取得，如何从资源事件走到结果与清理？ |
| 5 | [Init 协议](../../internal/sandboxsupervisor/init_linux.go) | 主进程退出、后代回收和输出完成如何衔接？ |
| 6 | [生命周期](../../internal/sandboxsupervisor/lifecycle_linux.go) | 资源清理失败为何还保留 Sandbox，下一次销毁如何继续？ |
| 7 | [公开结果](../../internal/sandboxsupervisor/protocol.go) | 哪些事实来自进程退出，哪些事实来自宿主资源计数？ |

先沿主流程读完一次正常执行，再挑内存超限和清理失败各走一遍错误分支。不要一开始就尝试背下所有辅助函数；更有用的阅读目标是回答“此时谁拥有哪个资源，下一步失败由谁处理”。

## 如何判断验收证据是否足够

资源控制必须在具有相应权限与内核功能的 Linux 环境中验证。配置解析测试可以证明输入校验，不能证明内核真的执行了预算；编译通过也不能证明进程归组与回收没有竞态。

仓库的 [Linux 执行验收入口](../../tests/run-sandbox-init-linux.sh) 会准备真实 Init 与 Python Profile；资源场景见 [sandbox_resource_linux_test.go](../../tests/sandbox_resource_linux_test.go)，配置与协议场景见 [sandbox_resource_protocol_linux_test.go](../../tests/sandbox_resource_protocol_linux_test.go)。运行前置条件沿用 [T05 的演示说明](t05-sandbox-init.md) 和 [Supervisor 协议与配置合同](../sandbox-supervisor-protocol-v1.md)。

阅读测试结果时，应分别寻找以下证据：

| 场景 | 需要确认的行为 | 为什么仅看输出不够 |
| --- | --- | --- |
| 正常短执行 | 得到普通退出结果，资源事件增量为零 | 仅能打印文字，不能证明配置生效 |
| 多进程 CPU 负载 | 确认配额设置，观察内核节流计数 | 总耗时也受宿主负载影响，不能单独用来认定配额正确 |
| 内存压力 | 有 cgroup OOM 证据、资源原因和后代清理结果 | 一个 MemoryError 或退出码 137 都不足以单独归因 |
| 持续创建任务 | PID 上限拒绝创建，平台随后终止整次 Execution | 创建失败不代表已有后代都已经结束 |
| 连续两次执行 | 旧 Workspace 内容仍受父预算约束，事件不串到下次结果 | 每次重新创建 Sandbox 会绕过这个生命周期问题 |
| 清理失败后重试 | 旧身份与资源所有权仍被保留，修复条件后能够重试 | 只检查 API 返回错误看不出是否泄漏资源 |

本次实现已在独立的 Linux 6.12.107、Go 1.26.1、2 vCPU／4 GiB 虚拟机中验证这些场景。普通用户的格式、静态分析、完整 Go 测试与构建通过；真实内核的 Sandbox 创建、Execution、生命周期和资源测试通过；Profile Bundle 安装、两次可复现构建及真实 Python 标准库验收也通过。审查后共享了两个验收入口的 cgroup 准备逻辑，并重新验证两个入口。

默认禁用 swap 与可信 swap 配置通过内核接口读回验证；本次虚拟机没有配置交换设备，因此没有声称完成“宿主启用 swap 后”的交换压力对照。测试探针使用有界分配和有限数量的派生进程；这些验证结果也不等于完整生产安全边界或容量基准。

## 面试时如何讲清楚这项工作

先讲原来的问题：项目已经能在同一 Sandbox 中连续执行 Python，但 namespace 并不能阻止 Workload 消耗过多资源。然后讲你增加的行为：Supervisor 配置 Sandbox 总预算，在执行前归组，依据内核事件分类结果，并沿原有生命周期清理后代。

接着选两个具体取舍深入讲。第一个是“为什么需要持续存在的父预算”：Workspace 是 tmpfs，文件可以比某次 Python 活得更久，预算因此必须跟随 Sandbox 的生命周期。第二个是“为什么 CPU、OOM、PID 不能采用同一判断”：CPU 配额触发节流，PID 上限拒绝创建任务，OOM 涉及分配失败与牺牲进程，平台必须分别定义后续行为。

常见追问可以用下面的问题自测：

1. `cpu.max` 的两个数字是什么单位？为什么第一个可以比第二个大？为什么它不能替代超时？
2. Python 已经退出，Workspace 文件还在；它占用的内存由哪一层预算继续承接？
3. 把 Init 放在 Execution 组里，`cgroup.kill` 会有什么后果？把 Init 放在预算外又有什么代价？
4. 为什么 launcher 需要 gate？如果先启动 Python 再迁移，可能漏掉哪些进程？
5. Init 的 OOM 保护为什么需要在 launcher 上撤销？即使保护设置成功，为何还不能保证 Init 必然存活？
6. PID 上限触发后，内核是否自动杀掉全部后代？Supervisor 补了什么策略？
7. 为什么不能用退出码 137 判断 OOM？为什么事件要计算本次增量？
8. 终止请求成功、组里没有活进程、Init 完成回收，分别证明了什么？
9. 清理失败时为什么保留身份和句柄？这项选择如何影响容量与重试？

回答时把代码、机制和证据连接起来。例如，“我用独立 Execution cgroup 清理全部后代”之后，还应能指出 gate 保证后代从正确组继承成员关系，以及测试如何确认组为空。这样描述的是你能解释、能验证的工程行为，而不是把内核术语堆成一行简历。
