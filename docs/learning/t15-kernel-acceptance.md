# T15：Sandbox 联合内核验收

T15 / [#21](https://github.com/TH1NKING/Build-your-own-Docker-with-Go/issues/21) 是 Sandbox 实现的验收汇总。我用同一套构建产物串起公开创建与执行入口，将输入、隔离、资源、结果和失败清理作为一个整体检查。它不引入新的运行功能，也不代替完整 Agent 平台的发布验收。

## 运行方式

专用 Linux 环境、锁定 Profile 源码缓存和已委派 cgroup v2 的前提与 [Supervisor 协议](../sandbox-supervisor-protocol-v1.md) 一致。从仓库根目录运行：

```bash
export PROFILE_BUNDLE_SOURCE_CACHE=/absolute/profile-sources
# 按环境需要设置，而不是指向一个未经委派的任意目录：
# export SANDBOX_TEST_CGROUP_PARENT=/absolute/delegated/cgroup
make test-sandbox-acceptance
# 没有 make 的精简环境可用：
bash tests/run-sandbox-acceptance-linux.sh
```

这两个入口等价，任选一个即可。脚本先构建共享 Supervisor、Init、Profile Bundle 和 tagged 测试二进制，运行 Execution/Lifecycle，再补充可信创建探针并用同一测试二进制运行 Creation。

已经构建的环境可以设置 `SANDBOX_TEST_BIN_DIR`，该目录还需包含 `sandbox-root-probe` 与 `sandbox-creation-tests`。修改代码后要重新构建，不能复用旧产物。协议、格式、静态检查、编译和 Shell 语法仍通过普通 Linux 用户运行的 `make check` 验证。

日志默认保留在 `.cache/sandbox-acceptance/run-<UTC时间>-<随机后缀>/`，也可设置 `SANDBOX_ACCEPTANCE_LOG_DIR`。目录包含 `environment.log`、`execution.log`、`creation.log` 和仅在成功后产生的 `result.log`；构建阶段失败时也保留相应日志。连续运行不会覆盖前一份结果。

门禁拒绝四种假通过：

- 任一 runner 失败，或日志写入失败；保留非零退出状态。
- 出现顶层或嵌套 `SKIP`。
- 零测试、缺顶层成功用例或缺最终 `PASS`。
- 旧测试二进制缺少必需的 T07 三项和 T13 九项顶层用例。脚本按精确名字检查，列出缺失项并要求重新构建。

环境中的 `SANDBOX_TEST_RUN` 不会把联合验收缩成一次局部检查。CI 的 Profile Bundle job 调用此入口，并无论成功失败都上传日志。

## 覆盖矩阵

| 合同 | 真实 Linux 证据入口 |
|---|---|
| 身份映射、只读根、旧根脱离 | `sandbox_creation_linux_test.go`，公开 create 与可信根探针 |
| 常驻 PID 1、顺序执行、后代回收 | `sandbox_execution_linux_test.go`、lifecycle 系列 |
| `/dev` 精确白名单、只读 proc 与固定掩码 | `sandbox_hardening_linux_test.go`，公开 Python 读取实际设备与 mountinfo |
| 目录、文件、socket、高编号 FD 不继承 | hardening/creation 测试检查 Init 与 Python 的真实 FD 集 |
| 固定 none 网络 | Workload syscall 拒绝；独立于 seccomp 的可信 netns 入站/出站探针，带宿主成功对照 |
| capabilities、no_new_privs、seccomp | `sandbox_security_linux_test.go`、`sandbox_policy_arguments_linux_test.go` |
| CPU、内存、swap、PID、tmpfs 配额 | resource/storage 系列，读取内核预算、计费或事件 |
| 执行期限与失联清理 | deadline/lifecycle 系列，包含真实 60 秒默认期限与更长可信配置 |
| 有界 stdout/stderr、结果快照 | output/result 系列，检查截断、保留和复读语义 |
| 只读输入、恶意来源拒绝、计费与失败重试 | attachment 与 attachment_edge 两个测试文件 |
| 声明输出、路径/内容竞态、类型与预算 | extraction 与 extraction_race 系列 |

## 2026-09-13 验证记录

环境为专用 QEMU Linux `6.12.107-0-virt`、amd64、Go `1.26.1`，使用 cgroup v2 `cpu memory pids`。内核支持 user/mount/PID/network namespaces、`pivot_root`、`clone3`/`CLONE_INTO_CGROUP`、`cgroup.kill`、seccomp、tmpfs 和所需只读 bind 行为。

| 验证 | 结果 |
|---|---|
| 完整 Execution/Lifecycle | 67 个顶层测试通过，0 跳过；其中 T13 9 项、T07 3 项 |
| 创建、根隔离与配置 | 10 个顶层测试通过，0 跳过 |
| Profile 安装与真实 Python Profile | 18 个顶层测试通过，0 跳过；包含两次可复现构建与新增 `/dev` 保留路径拒绝 |
| 普通 UID 1000 的格式、vet、test、build、全部 Shell 语法 | 通过；未配置数据库时，PostgreSQL 专项明确跳过，不计入 Sandbox 联合内核证据 |
| 三组文件演示 | normal、read-only、malicious 均通过 |
| 验收脚本负对照 | 旧套件、缺必需用例、跳过和零测试均不能产生 T15 PASS；失败状态保留 |

内核日志中的 OOM kill、权限拒绝或 cleanup_failed 诊断可能来自专门的失败注入；应看对应测试是否观察到预期行为、是否完成清理，以及最终 suite 状态，不能仅搜索日志中的 error 单词判断成功失败。

旧实现的负对照确认了两项实际缺口：新增输入用例因缺少 `--attachment-root` 失败，内核视图用例因缺少 `/dev` 失败。实现后同样的公开入口用例通过。网络验收还纠正了一项测试假设：Linux 新 netns 没有主路由表时，`/proc/net/route` 可以直接返回空文件。因此测试接受空文件或精确表头，但拒绝任何路由数据行；入站、出站和接口检查没有放松。

本地 QEMU 验证日志保存在 `.cache/t09-vm/results/` 下的 `t15-final/`、`t15-profile.log`、`t15-standard.log` 与 `t13-demo-*.log`；这些可再生成的本地日志不进入源码版本控制。公开 CI 运行的日志由 Actions artifact 保存。

## 结论的适用范围

这些是特定 Linux 环境中的功能、隔离边界与失败路径证据，不是性能压测，也不是所有内核版本的安全认证。单次通过不保证不存在竞态或内核漏洞；共享内核仍是信任边界。

T13 的输入暂存者是可信的，需保持源 inode 内容不变；只读挂载不限制可信宿主 root 的其他写入路径。有限 proc 掩码不隐藏全部宿主统计。Supervisor 跨重启恢复、Conversation 授权、ArtifactStore、Worker 租约与完整 Agent Loop 属于后续合同。

复述项目时可以说：“我实现了只读文件输入、受限执行和声明输出提取，并在真实 Linux 上验证了非法文件来源拒绝、资源限制和失败清理。”应把这些已经验证的本地能力与尚未实现的完整平台能力分别说明。
