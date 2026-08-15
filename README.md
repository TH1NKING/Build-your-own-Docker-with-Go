# Build Your Own Docker with Go

这个仓库以一个可运行的 Linux Runtime Lab 为起点，逐步实现面向 Agent 的安全代码执行平台。当前 Go 与 Shell 实现都是学习和验证 Linux 容器原语的实验代码，不是生产级容器运行时或安全沙箱。

> **安全边界：** Runtime Lab 会以 root 权限操作 namespace、OverlayFS、cgroup 和宿主机网络。只应在专用 Linux 虚拟机或其他可丢弃环境中运行；不要用它执行不受信任的代码。

## 仓库结构

- `cmd/runtime-lab`：Go Runtime Lab 的标准命令入口。
- `internal/runtimelab`：标准入口与兼容入口共享的实验实现。
- `with_Go`：保留原有调用方式的兼容入口。
- `with_shell`：Shell 版对照实验；仅用于教学和行为比较。
- `tests`：面向公开命令入口的 Linux 验收测试。
- `docs/adr` 与 `CONTEXT.md`：目标系统的架构决策和上下文文档。

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
