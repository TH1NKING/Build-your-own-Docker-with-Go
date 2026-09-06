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

Profile Bundle 另有一个 Linux root 验收 job：它校验锁定下载、两次可复现构建、root-owned 原子安装、恶意 Bundle 拒绝以及真实 CPython 内容 smoke。格式和命令合同见 [`docs/profile-bundle-v1.md`](docs/profile-bundle-v1.md)。

Sandbox Supervisor 协议测试必须以普通 Linux 用户运行，通过真实 Unix socket 验证版本、封闭 operation 集合、严格消息解析、受信路径和权限边界。T04 已实现创建真实 Sandbox：配置预留的 subordinate UID/GID 后，`sandboxd` 建立独立 user/mount/PID/network namespace，将 Runtime Profile 作为只读私有根挂载，通过 `pivot_root` 脱离旧根，再返回 `sandbox_id`。三个 subordinate-ID 参数全部为零时保留仅验证协议的模式，创建仍返回 `operation_unavailable`。配置与协议合同见 [`docs/sandbox-supervisor-protocol-v1.md`](docs/sandbox-supervisor-protocol-v1.md)。

在已安装 Go、`sudo` 和 util-linux 的专用、可丢弃 Linux 环境中，以普通用户运行真实创建验收：

```sh
bash tests/run-sandbox-creation-linux.sh
```

这个创建验收入口构建工具和可信测试探针，再在隔离的 Linux 测试环境中验证身份映射、只读根、旧宿主根不可达和描述符边界。T04 探针通过 `nsenter` 启动；该阶段的原理与实验记录保留在 [`T04 学习笔记`](docs/learning/t04-sandbox-creation.md)。

T05 已接入 Profile Bundle 中的真实 Sandbox Init：可信 PID 1 通过私有继承管道处理顺序 Python Execution，Workload 使用内部 UID/GID 1000，共享同一 Workspace；每次执行结束后先终止并回收后代，再允许复用。准备经过摘要校验的源码缓存后，可运行 `PROFILE_BUNDLE_SOURCE_CACHE=/absolute/path/to/cache bash tests/run-sandbox-init-linux.sh` 验证真实 CPython 连续执行。设计取舍、客户端演示与测试证据见 [`T05 学习笔记`](docs/learning/t05-sandbox-init.md)。T06 的完整生命周期与恢复、T07–T12 的后续安全及资源合同仍需完成，当前不能据此用于任意不可信 Workload。

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
