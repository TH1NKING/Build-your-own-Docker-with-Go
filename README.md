# Build Your Own Docker with Go

这个仓库以一个可运行的 Linux Runtime Lab 为起点，逐步实现面向 Agent 的安全代码执行平台。当前 Go 与 Shell 实现都是学习和验证 Linux 容器原语的实验代码，不是生产级容器运行时或安全沙箱。

> **安全边界：** Runtime Lab 会以 root 权限操作 namespace、OverlayFS、cgroup 和宿主机网络。只应在专用 Linux 虚拟机或其他可丢弃环境中运行；不要用它执行不受信任的代码。

## 仓库结构

- `cmd/runtime-lab`：Go Runtime Lab 的标准命令入口。
- `internal/runtimelab`：标准入口与兼容入口共享的实验实现。
- `with_Go`：保留原有调用方式的兼容入口。
- `with_shell`：Shell 版对照实验；仅用于教学和行为比较。
- `tests`：面向公开命令入口的 Linux 验收测试。
- `cmd/profile-bundle`：构建和 root-owned 安装 `python-data-v1` Profile Bundle 的公开工具。
- `cmd/sandboxd`：Linux 上的特权 Sandbox Supervisor 入口；仅开放受限本地协议，不接受通用宿主机命令或路径。
- `cmd/agentctl`：Control Plane 运维入口，当前提供 PostgreSQL 版本化迁移和只读版本检查。
- `profiles/python-data-v1`：锁定的 Runtime Profile 输入与候选 System Call Policy。
- `docs/adr` 与 `CONTEXT.md`：目标系统的架构决策和上下文文档。
- [`CONTRIBUTORS.md`](CONTRIBUTORS.md)：项目贡献者与协作者署名。

## 贡献者与协作者

感谢所有帮助这个项目成长的人与协作工具；完整署名见 [`CONTRIBUTORS.md`](CONTRIBUTORS.md)。

## Linux 前置条件

- 构建与测试支持基线是 GitHub-hosted `ubuntu-latest`；其他 Linux 发行版需要提供等价工具。
- Go 版本以 [`go.mod`](go.mod) 为准。
- Linux 内核支持 user/mount/PID/network namespace、OverlayFS 和 cgroup v2。
- 命令行提供 `bash`、`ip`、`iptables`、`mountpoint` 和 `unshare`。
- 实际启动 Runtime Lab 时需要 root 权限，并在当前工作目录准备 `rootfs/`。

## 构建与验证

在干净的 Linux checkout 根目录运行：

```sh
make check
```

该命令依次检查 Go 格式、静态分析、测试、构建和 Shell 语法。也可以分别运行：

```sh
go vet ./...
go test ./...
go build ./...
bash -n with_shell/*.sh
```

GitHub Actions 会在 Ubuntu 上对 push 和 pull request 执行同一个 `make check`，因此本地与 CI 使用相同的验收入口。

T17 增加 `agentctl migrate`：在 PostgreSQL 17 上逐文件事务化应用迁移，将 SQL 变更与版本记录一起提交；重复执行报告当前版本，并校验已应用文件的名称与 SHA-256。并发命令通过数据库 advisory lock 串行执行，超时主动取消 SQL，失败后保留已提交版本。第一份迁移只建立 `control_plane` schema，业务表由后续 ticket 引入。命令、环境配置和新增迁移流程见 [`迁移合同`](docs/agentctl-migrations.md)，原理与实验见 [`T17 学习笔记`](docs/learning/t17-postgresql-migrations.md)。

设置指向专用测试 PostgreSQL 的 `AGENT_TEST_DATABASE_URL` 后，运行 `make test-migrations` 执行真实 CLI 验收；该入口缺少配置时会失败。普通 `go test ./...` 未配置数据库时会明确跳过数据库用例。CI 有独立 PostgreSQL 17 service job，覆盖初始化、重复运行、只读状态、中途回滚、历史校验、并发和超时恢复。

Profile Bundle 另有一个 Linux root 验收 job：它校验锁定下载、两次可复现构建、root-owned 原子安装、恶意 Bundle 拒绝以及真实 CPython 内容 smoke。格式和命令合同见 [`docs/profile-bundle-v1.md`](docs/profile-bundle-v1.md)。

Sandbox Supervisor 协议测试必须以普通 Linux 用户运行，通过真实 Unix socket 验证版本、封闭 operation 集合、严格消息解析、受信路径和权限边界。T04 已实现创建真实 Sandbox：配置预留的 subordinate UID/GID 后，`sandboxd` 建立独立 user/mount/PID/network namespace，将 Runtime Profile 作为只读私有根挂载，通过 `pivot_root` 脱离旧根，再返回 `sandbox_id`。三个 subordinate-ID 参数全部为零时保留仅验证协议的模式，创建仍返回 `operation_unavailable`。配置与协议合同见 [`docs/sandbox-supervisor-protocol-v1.md`](docs/sandbox-supervisor-protocol-v1.md)。

在已安装 Go、`sudo` 和 util-linux 的专用、可丢弃 Linux 环境中，以普通用户运行真实创建验收：

```sh
bash tests/run-sandbox-creation-linux.sh
```

这个创建验收入口构建工具和可信测试探针，再在隔离的 Linux 测试环境中验证身份映射、只读根、旧宿主根不可达和描述符边界。T04 探针通过 `nsenter` 启动；该阶段的原理与实验记录保留在 [`T04 学习笔记`](docs/learning/t04-sandbox-creation.md)。

T05 已接入 Profile Bundle 中的真实 Sandbox Init：可信 PID 1 通过私有继承管道处理顺序 Python Execution，Workload 使用内部 UID/GID 1000，共享同一 Workspace；每次执行结束后先终止并回收后代，再允许复用。准备经过摘要校验的源码缓存后，可运行 `PROFILE_BUNDLE_SOURCE_CACHE=/absolute/path/to/cache bash tests/run-sandbox-init-linux.sh` 验证真实 CPython 连续执行及 T06 生命周期。T05 的设计与测试记录见 [`T05 学习笔记`](docs/learning/t05-sandbox-init.md)。

T06 增加公开 `DestroySandbox` 调用，并处理执行中断连、Init 丢失、创建阶段失败及服务正常关闭时的清理。销毁完成后才能释放身份范围；已结束的 Sandbox ID 在本次 Supervisor 运行期间不可复用。清理遇到陌生文件或被替换的目录会返回 `cleanup_failed`，保留现场供修复后重试。完整客户端示例、设计取舍和测试证据见 [`T06 学习笔记`](docs/learning/t06-sandbox-lifecycle.md)。Supervisor 崩溃后的持久恢复不在 T06 范围内。

T08 在 Python 启动前清空 Workload 的 capability 集合、设置不可撤销的 `no_new_privs`，并安装与 Profile Bundle 摘要绑定的 seccomp 白名单。策略仅支持 Linux amd64，检查完整 64 位参数，拒绝 namespace 创建、未审阅的 `clone` 标志和其他 syscall ABI；禁止调用返回 `EPERM`。真实 Linux 验收覆盖 Python 数据处理、线程与子进程继承、参数边界、策略篡改和拒绝后的复用。权限设置时机、白名单兼容问题和方案取舍见 [`T08 学习笔记`](docs/learning/t08-system-call-policy.md)。

T09 使用持续存在的 Sandbox cgroup v2 父组执行 CPU、内存、swap 和 PID 资源预算，Init 与每次 Execution 使用独立子组。默认预算为 2 CPU、1 GiB 内存、禁用 swap、64 个内核任务，均由可信启动配置决定。内核 CPU 节流与内存/PID 超限分别报告；跨 Execution 留存的 Workspace 内存继续计入父预算。运行验收需要已委派 `cpu memory pids` 控制器的 cgroup v2 父目录，可通过 `SANDBOX_TEST_CGROUP_PARENT` 指定。原理、方案比较与代码阅读路线见 [`T09 学习笔记`](docs/learning/t09-resource-budgets.md)。

T12 增加每次 Execution 的可信执行期限，默认 60 秒，可通过 `sandboxd --execution-timeout` 配置。超时后终止该次执行的全部后代，等待 Init 回收并移除 Execution cgroup，再返回 `timed_out`；清理成功时同一 Init 和 Workspace 可供后续执行复用。客户端取消和显式销毁仍终止整个 Sandbox。实现原理、替代方案、竞态与验证方法见 [`T12 学习笔记`](docs/learning/t12-execution-deadlines.md)。

T11 将 stdout、stderr 的默认捕获额度分别设为 1 MiB，支持可信启动配置、独立截断标记与有界结果快照。超出额度的输出继续被读取并丢弃；`GetExecutionResult` 重复读取已经完成的结果，不重新执行代码。结果暂存有条目数和总预留字节上限，显式销毁 Sandbox 时释放，Supervisor 重启后不保留。协议传输、并发管道、生命周期和替代方案见 [`T11 学习笔记`](docs/learning/t11-bounded-execution-results.md)。

T10 将 Workspace 与 `/tmp` 的存储预算纳入可信启动配置：默认分别为 512 MiB / 5,000 个文件名额和 16 MiB / 1,023 个文件名额。独立 tmpfs 在执行中拒绝超额分配，目录、链接和仍打开的已删除文件继续占用相应额度；状态和占用跨 Execution 保留，释放后可复用。字节预算计算实际分配的数据页，稀疏文件的逻辑长度及输出提取仍需单独限制。配置、内核计费、与 cgroup OOM 的区别、方案取舍及实验见 [`T10 学习笔记`](docs/learning/t10-storage-budgets.md)。T07 等后续安全合同仍需完成，当前不能据此用于任意不可信 Workload。

## 使用 Go Runtime Lab

查看标准入口和兼容入口的参数：

```sh
go run ./cmd/runtime-lab --help
go run ./with_Go --help
```

在已经准备好 `rootfs/` 的专用 Linux 环境中启动实验容器：

```sh
sudo go run ./cmd/runtime-lab [-m 100M] [-c 20] <IP> [COMMAND...]
```

例如：

```sh
sudo go run ./cmd/runtime-lab -m 100M -c 20 10.200.1.2 /bin/sh
```

运行产生的 `containers/` 和 `rootfs/` 是本地可变状态，不纳入版本控制。

## Shell 对照实验

Shell 版本保留用于观察相同 Linux 原语的组合方式：

```sh
sudo bash with_shell/net-setup.sh
sudo bash with_shell/run-mini-container.sh 10.200.1.2 /bin/sh
```

它没有生产级输入验证、隔离强化或故障恢复保证，不能作为安全边界。
