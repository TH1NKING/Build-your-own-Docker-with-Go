# T14：把声明的输出文件变成有界快照

前面的 Execution 已经可以返回 stdout、stderr，也能把文件留给同一 Sandbox 中的下一次执行。我做 T14，是为了让调用方明确取回某几个文件，同时把“不该读的文件”和“读不完的文件”挡在提取边界。对应 [Issue #20 / T14](https://github.com/TH1NKING/Build-your-own-Docker-with-Go/issues/20)，目标来自 [ADR-0022](../adr/0022-upload-only-declared-execution-outputs.md)。

这次增加的是本地 Execution Result 中的文件字节快照。`execute_python` 声明 `output_paths`，成功时返回每个文件的路径、大小、SHA-256 和二进制内容；未声明的文件仍留在 Workspace。后续 Worker 上传、Artifact Store、OSS 和 Run Artifact 持久化还没有实现，不能把这一步写成已经完成产物分发平台。

## 调用方能观察到什么

下面是已有客户端和 Sandbox 上的一次调用片段。`client`、`ctx` 和创建阶段得到的 `sandboxID` 沿用 [T06 客户端示例](t06-sandbox-lifecycle.md)。每次执行需要新的 Execution ID。

```go
response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
    RequestID:   "req-extract-1",
    SandboxID:   sandboxID,
    ExecutionID: "extract-1",
    Source: `from pathlib import Path
Path("answer.txt").write_bytes(b"abc")
Path("scratch.txt").write_text("keep for the next Execution")
print("saved")`,
    OutputPaths: []string{"answer.txt"},
})
if err != nil {
    // 传输结果不确定时，不能直接重跑 Workload。
    return err
}
if response.Error != nil {
    return fmt.Errorf("execute_python: %s", response.Error.Code)
}
result := response.Result
if result.OutputError != "" {
    return fmt.Errorf("file extraction: %s", result.OutputError)
}
for _, output := range result.Outputs {
    fmt.Printf("%s: %d bytes, sha256=%s\n", output.Path, output.Size, output.SHA256)
    // output.Content 已经是解码后的 []byte。
}
```

`answer.txt` 的快照是三个字节 `abc`；JSON 中 `content` 为 `YWJj`。`scratch.txt` 没有声明，所以不会被扫描、读取或返回，但下一次 Execution 仍能读取它。省略 `OutputPaths` 或传入空列表，继续得到原来的执行与标准流结果，不做文件提取。

`GetExecutionResult` 读取已保存的快照。如果下一次 Execution 把 `answer.txt` 改成别的内容或删掉，前一次结果仍是原来的三个字节。重复读取不会重新执行 Python、重新打开 Workspace 文件或再次消耗累计提取额度。Supervisor 重启会丢失这些暂存结果；显式销毁 Sandbox 会释放对应结果。

## 我把执行结果和提取结果分开了

文件没有安全取出，不代表 Python 没有运行；Python 返回非零，也不代表它没有留下可以取出的诊断文件。因此 `output_error` 是 Execution Result 内的字段，不覆盖已有退出码、资源终止原因和标准流。

| 观察到的情况 | 接口行为 |
| --- | --- |
| 没有声明文件 | 不返回 `outputs` 或 `output_error` |
| 全部声明文件通过检查 | 按声明顺序返回 `outputs`，不返回 `output_error` |
| 文件不存在、链接、特殊文件、不安全解析或观察到变化 | `output_error=unsafe_output`，整批不返回文件 |
| 任一文件超出单文件、单次或累计剩余额度 | `output_error=output_limit`，整批不返回文件 |
| 已有可信资源终止结果，但 Init 丢失、声明文件无法取回 | `output_error=output_unavailable`，不制造成功的空结果 |
| 请求本身含不合法路径或超出声明数量 | 执行前返回协议错误 `invalid_reference` |

例如一次执行打印了 `done` 并正常退出，随后提取发现其中一个声明文件是符号链接：`exit_code` 仍是 `0`，`terminal_reason` 仍是 `exited`，stdout 仍保留 `done`，但文件列表整体不返回。调用方需要分别检查执行与提取状态。

整批失败也意味着前几个已经检查通过的文件不会偷偷变成部分成功结果，不会扣除累计成功快照字节。这个选择让当前接口和计费都较简单；代价是一个错误路径会使同批正常文件也取不到。未来若需要部分成功，应明确增加逐文件结果合同，而不是让调用方猜测少了哪些文件。

## 路径检查只是入口，读取要跟着描述符走

声明必须是 `/workspace/output` 下的相对文件路径。入口拒绝绝对路径、`.`、`..`、穿越、反斜杠、NUL、重复分隔符、尾部分隔符和同批重复路径；每个路径最多 1,024 个 UTF-8 字节，总请求仍受 64 KiB 帧限制。可以声明 `plots/result.png`，不能导出目录；`*` 等字符按文件名原样处理，不做通配符展开。

只做 `Clean`、字符串前缀检查或先 `Stat` 再 `ReadFile`，仍无法确定实际读到的对象。检查时的路径可以指向普通文件，读取时却已经被替换；中间目录也可能是链接。文件描述符在名字被删除或替换后仍引用原先打开的对象，因此它适合作为后续操作的锚点。[Linux `open(2)`](https://man7.org/linux/man-pages/man2/open.2.html)

实际提取发生在可信 Sandbox Init 内，顺序是：

1. 用 `O_PATH | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC` 固定 `/workspace/output`，记录所在设备。
2. 从这个目录描述符开始，对每个路径分量调用 `openat`；每一级都带 `O_NOFOLLOW`，中间分量还必须是目录。
3. 对叶子执行 `fstat`，只接受同一设备上的普通文件，并要求 `nlink == 1`。
4. 确认类型和逻辑长度后，用 Init 自己的 `/proc/self/fd/<已固定的fd>` 获得可读句柄。这里不再打开调用方的原路径。
5. 读取固定长度并确认 EOF，再检查描述符元数据，重新解析原路径并核对各级目录与叶子的身份。

`O_PATH` 先拿到对象的路径句柄而不读取其内容，这让 FIFO 或设备在检查类型前不会进入普通内容读取流程。`O_NOFOLLOW` 对一次打开的最后分量生效，所以代码逐段解析，不能只给完整路径末尾加一个标志就忽略中间目录。[Linux `open(2)` 的 `O_PATH` 与 `O_NOFOLLOW` 说明](https://man7.org/linux/man-pages/man2/open.2.html)

这里的 `/proc/self/fd` 是可信 Init 自己持有的描述符入口，用来读取已确认的普通文件，不接受 Workload 提供的 `/proc` 路径。打开它仍受内核权限检查，不是任意绕过权限的方法。[Linux `proc_pid_fd(5)`](https://man7.org/linux/man-pages/man5/proc_pid_fd.5.html)

硬链接也需要明确处理。它不是符号链接，单靠 `O_NOFOLLOW` 不会拒绝；本实现要求普通文件的链接数恰为 1，拒绝有多个名字的 inode，而不是尝试扫描整个 Workspace 证明其他名字安全。这个规则限制了可导出的文件形式，也减少了别名分析的复杂度。

## 先让写入者退出，再检查文件是否稳定

描述符固定了对象，却不会让对象内容自动只读。如果另一个进程仍在写，同一个 fd 也可能读到不断变化的数据。比较两次大小也不够：文件可以被同长度改写，或者中间目录被移动后仍由旧描述符持有。

我把提取接在现有收尾流程里：Supervisor 终止 Execution cgroup 并等待没有存活任务，通知 Init 回收所有后代、排空标准流，随后 Init 才开始提取。这一整段仍持有 Sandbox 的 Execution 锁，下一次 Execution 不能抢先启动。主 Python 进程退出、仅等待一个 PID、仅发送信号，都不能替代这个条件。

在这个生命周期条件之上，再做内容和路径复核：先记录逻辑长度，读取恰好这么多字节，并要求下一字节读取立即得到 EOF；读完比较设备、inode、模式、链接数、UID/GID、大小、mtime 和 ctime。最后重新解析原路径，确认各级目录和叶子的身份仍对应原来的对象。元数据中每个字段的含义见 [Linux `stat(2)`](https://man7.org/linux/man-pages/man2/stat.2.html)。

这些检查用于拒绝观察到的变更，不能单独证明“面对任意并发写入者仍获得原子快照”。本项目的稳定性依赖前面的后代回收、顺序执行和受保护的 Workspace 结构；可信宿主机管理员主动修改运行环境不属于这里防御的 Workload 边界。收尾或 Init 握手不确定时，继续沿用 Sandbox 失效与清理流程，不允许带着未知写入者复用。

SHA-256 在内容读取并复核后计算，Supervisor 还会核对 Init 返回的路径顺序、大小、摘要和预算。摘要用于识别拿到的这份字节，不能证明文件没有恶意内容，也不能替代路径、类型、权限或生命周期检查。

## 三种字节额度解决三个不同问题

T10 的 tmpfs 预算限制已经分配的数据页与文件名额。T14 限制读出来的逻辑字节；T11/T14 的结果暂存预算限制 Supervisor 保留和预留的结果容量。它们的计费对象不同。

| 提取配置 | 默认值 | 约束 |
| --- | --- | --- |
| `--extract-file-bytes` | 20 MiB | 正整数，固定协议上限 32 MiB |
| `--extract-execution-bytes` | 32 MiB | 正整数，固定协议上限 32 MiB |
| `--extract-sandbox-bytes` | 100 MiB | 正整数，同一个 Sandbox / Agent Run 累计 |
| `--extract-files` | 16 | 1 到固定上限 16，每次 Execution 的声明数量 |

这些都是 Operator 的可信启动配置，公开请求只能声明路径，不能提高额度。字节数无需页对齐；零不会表示无限制。Init 私有请求也检查单文件和当次剩余额度。

一次声明批次的总可用量为：

```text
min(单次 Execution 字节上限,
    Sandbox 累计上限 - 已成功提取字节,
    声明数量 × 单文件字节上限)
```

单个文件还受单文件上限约束。只有完整成功的快照才累计原始字节；同一文件在新的 Execution 中再次声明，会再次收费。同一次请求里的重复路径在执行前被拒绝。读旧结果不收费，删掉 Workspace 原文件不退还累计提取量；空普通文件收费零字节，所以累计剩余额度为零时仍可以提取空文件。

稀疏文件是区分这些预算的好例子。Workload 可以把一个文件的逻辑长度扩展得很大，但只分配少量 tmpfs 数据页；如果提取端直接 `ReadFile`，洞会展开成大量零字节。本实现先检查 `st_size`，再分配有界缓冲区。T10 的分配容量因此不能替代这里的逻辑长度检查；具体 tmpfs 计费机制和实验见 [T10 学习笔记](t10-storage-budgets.md)。

## 为什么先返回有界字节，而没有直接做流式上传

当前一次 Execution 的文件总量最多 32 MiB。我选择先完成不可变快照，再通过已有 JSON 协议返回：调用方拿到的 size、digest 和 content 是同一份数据，失败可以整批丢弃，`GetExecutionResult` 也有明确的重复读取语义。二进制走 base64，stdout/stderr 继续作为有损文本独立截断。

代价是暂存、编码和解码都会占内存，base64 也会扩大传输量。这不是大文件传输方案。直接流式上传可以降低完整文件驻留需求，但还要处理上传过程中的变更、半成品对象、取消、摘要确认、重试与最终提交；这些工作属于后续 Worker 和 Artifact Store 集成，需要单独设计。

协议请求仍最多 64 KiB，结果帧上限增至 160 MiB + 128 KiB。这个值是对最坏 JSON 文本转义、base64 文件内容和有界路径元数据的保守容纳上限，不是默认每次返回这么多字节。接收方在分配前拒绝超大声明帧。

Supervisor 全局最多保留 64 个结果槽位、128 MiB 的结果字节预留。开始执行前，先为 stdout/stderr 的可信额度及本批潜在提取量预留；完成后只释放未使用的提取预留，标准流仍保留原先额度。文件快照按实际原始字节留下，不能因为复用已有结果就绕开预留。

全局另外限制最多两个公开响应同时编码和写出，防止大量客户端同时重读一个大快照造成编码副本无限增长。等槽位和实际写出分别有五秒限制；响应传输失败不会自动重跑 Workload。

**128 MiB 不是 Supervisor 总 RSS 上限。** 进程还有元数据、Go 运行时、JSON 编解码和暂时传输副本；Init 也有自己的缓冲区。这里能说明的是结果预留、单帧长度和响应并发都有界，没有据此得出峰值内存或吞吐性能结论。

## 代码阅读路线

| 顺序 | 代码 | 我建议先回答的问题 |
| --- | --- | --- |
| 1 | [公开类型](../../internal/sandboxsupervisor/protocol.go)、[客户端](../../internal/sandboxsupervisor/client_linux.go) | `OutputPaths` 怎样到达协议？为什么提取失败是结果字段？ |
| 2 | [预算](../../internal/sandboxsupervisor/extraction_budget_linux.go)、[默认值](../../internal/sandboxsupervisor/resource_budget_linux.go)、[CLI](../../cmd/sandboxd/main_linux.go) | 默认值、固定上限和剩余额度分别由谁决定？ |
| 3 | [Execution](../../internal/sandboxsupervisor/execution_linux.go) | 什么时候拿锁、预留结果和累计成功字节？Init 丢失时怎样标记？ |
| 4 | [Sandbox Init](../../internal/sandboxsupervisor/init_linux.go) | 为什么必须等待 `reap` 和 `ECHILD`，再提取？ |
| 5 | [提取实现](../../internal/sandboxsupervisor/extraction_linux.go) | 路径语法、目录描述符、文件类型、长度、身份与摘要分别检查什么？ |
| 6 | [结果暂存](../../internal/sandboxsupervisor/results_linux.go)、[帧](../../internal/sandboxsupervisor/framing_linux.go)、[服务](../../internal/sandboxsupervisor/server_linux.go) | 如何避免结果和并发编码无限占内存？为什么读取不碰原文件？ |
| 7 | [真实提取验收](../../tests/sandbox_extraction_linux_test.go)、[竞态验收](../../tests/sandbox_extraction_race_linux_test.go) | 是否真的经过公开客户端、实际 Python、进程收尾和再次读取？ |

## 怎样验证，本次实际跑了什么

在具备 Go、非交互 sudo、锁定 Profile 源码缓存与可委派 cgroup v2 的专用 Linux 环境中，沿用 [Supervisor 协议文档](../sandbox-supervisor-protocol-v1.md) 的准备步骤。更新 Supervisor 时需要重新构建并安装包含匹配 T14 Init 的 Profile Bundle；旧 Init 不理解新的私有字段。

```sh
make check

PROFILE_BUNDLE_SOURCE_CACHE=/absolute/path/to/cache \
SANDBOX_TEST_CGROUP_PARENT=/sys/fs/cgroup \
SANDBOX_TEST_RUN='^TestSandboxExecutionExtraction' \
bash tests/run-sandbox-init-linux.sh
```

其中 cgroup 路径和源码缓存路径应换成实际已经配置好的环境。Windows 构建只能说明可编译，不能证明 `openat`、文件类型、后代终止或 cgroup 行为。普通协议测试也不能代替真实内核验收。

2026-09-10，我在专用 QEMU 环境的 Linux `6.12.107-0-virt`、Go `1.26.1` 上完成以下验证。环境提供真实 User/Mount/PID/Network namespace、cgroup v2 和 tmpfs，使用重新构建的 Sandbox Init 与锁定 Python Profile Bundle。该 guest 没有 `make`，所以通用检查以普通 UID 1000 逐项执行 Makefile 的同等命令。

| 验证范围 | 实际覆盖 | 结果 |
| --- | --- | --- |
| 公开协议与启动配置 | 合法声明；路径穿越、非规范路径、重复声明、字段类型、声明数量、预算覆盖及启动边界 | T14 的 4 个顶层测试通过 |
| 文件提取 | 已声明字节/大小/摘要、未声明文件留存、符号/硬链接、目录、FIFO 和 Unix socket 拒绝 | 完整套件中通过 |
| 预算与失败语义 | 单文件、单次与累计预算，稀疏文件，空文件，失败整批不发布且不计费，全局结果预留与销毁释放 | 完整套件中通过 |
| 稳定性与读取 | 后代回收，Workspace 改写后的旧快照，客户端修改返回值后的再次读取，重复读取及销毁 | 完整套件中通过 |
| 可复现竞态 | Linux 写 lease 的 SIGIO 作为屏障；正常对照、文件替换、同长度内容修改、父目录移动；提取中拒绝重叠执行，暂不发布部分结果 | 4 个子用例通过，无跳过 |
| 最大响应 | 32 MiB 文件、stdout/stderr 各 8 MiB，包含最坏文本转义与二进制内容 | 原有预算与传输超时下通过 |
| Execution／生命周期完整回归 | 包含 T14 新增的 8 个顶层内核测试及此前资源、安全、超时、清理合同 | 55 个顶层测试通过，0 跳过 |
| 创建隔离完整回归 | 真实身份映射、根隔离、只读挂载与描述符等既有验收 | 10 个顶层测试通过，0 跳过 |
| Makefile 等价检查 | gofmt、`go vet ./...`、`go test -v ./...`、`go build ./...` 和全部现有 Shell 语法检查 | 通过；数据库验收按未配置测试 DSN 明确跳过 |

本地原始日志保存在未纳入版本控制的 `.cache/t09-vm/results/t14-full-kernel.log`、`t14-creation.log` 与 `t14-native-check.log`；协议专项记录为 `t14-protocol-suite.log`。测试先后观察到声明字段、非法路径和真实提取的失败，再完成对应实现。双轴审查结果为 Standards 0 项、Spec 0 项。

测试夹具也遇到两个内核前置细节：宿主 UID 0 未映射到 Workspace tmpfs 的所属 User Namespace，直接创建 inode 或移动目录会返回 `EOVERFLOW`；`nsenter --wd` 又会在切换根之前打开目录。我让可信测试探针进入相应 namespace 后使用绝对 Workspace 路径完成故障注入，没有修改生产权限或放宽 System Call Policy。FIFO 与 Unix socket 的注入单独验证了提取端的类型检查。

本轮没有单独注入设备节点或跨设备挂载；对应拒绝分支来自普通文件类型和设备身份检查，不能写成这些独立内核用例已运行。未配置 `AGENT_TEST_DATABASE_URL`，所以 PostgreSQL 专项没有在本轮复验；本次也没有修改迁移实现。上述结果是功能与边界验收，不是性能压测或生产安全结论。T07 的完整挂载/描述符/网络合同、T13 的真实 Attachment 绑定和 T15 的整体安全验收仍需完成；ArtifactStore 从 T16 开始引入，本次输出快照也没有 Supervisor 跨重启恢复能力。

## 面试里我会怎样讲这部分

我会从“执行代码后如何安全取回指定文件”讲起，再沿着三条边界解释实现：路径解析固定对象，进程收尾固定写入时机，预算限制逻辑字节和结果留存。只说“过滤了 `../` 并计算哈希”解释不了中间目录、硬链接、特殊文件和并发改写。

对替代方案，我会说明为什么现在采用完整有界快照，以及什么时候容量和上传需求会促使我改成流式提取与分阶段提交。对验证，我会区分公开协议、真实 Linux 验收和性能实验，只引用最终实际跑过的记录。当前可以讲清楚本地文件提取的设计和边界；持久化 Run Artifact 和跨节点传输要等后续实现与验证后再写进简历。
