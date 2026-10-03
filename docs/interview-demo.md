# 面试演示：从简历主张到可复现证据

这份演示对应本仓库的「Go 自研容器运行时与 Agent 代码执行沙箱（原型）」。在专用 VMware Ubuntu x86_64 虚拟机中，先完成准备和整体验收，再保留相同 checkout 与构建产物用于现场演示。入口使用真实 PostgreSQL、HTTPS、Supervisor 和 Python；终端里的 `DEMO` 记录分步观察和故障注入，最外层入口的最终成功标记才表示整组完成。

## 简历中的每句话怎么证明

| 简历主张 | 演示 / 验收入口 | 可以解释的实现与边界 |
| --- | --- | --- |
| namespace、cgroup v2、OverlayFS、veth/bridge 的类 Docker 运行时 | `make demo SCENARIO=runtime` | `internal/runtimelab/run_linux.go` 是教学运行时；真实验证 PID 1、bridge ping、OverlayFS 写入/恢复、退出码和清理。强化 Sandbox 使用只读 Profile 和禁网策略，不能混称为带网络的 Docker 替代品 |
| 非特权 Worker / 特权 Supervisor、受限 Unix socket、固定 Profile | `make demo SCENARIO=worker`、`SCENARIO=security` | Worker CLI 以真实非 root 身份运行，使用私有 CA；Supervisor 只接受封闭操作，Workload 无权改预算或策略 |
| Sandbox / Execution 两级生命周期、Workspace 保留、后代回收后复用 | `make demo SCENARIO=lifecycle` | 相同 Init 和 Workspace 连续执行，超时后独立 Execution cgroup 消失；完整 Agent Loop 的多步调度尚未实现 |
| 只读输入、声明输出、SHA-256 | `make demo SCENARIO=files` | CSV 三行合计 25、24 字节结果快照、六种输入修改拒绝、恶意来源创建前拒绝；提取基于目录 FD，无文件下载界面 |
| 内存、CPU、PID、存储与输出预算 | `make demo SCENARIO=resources` | 实际触发 OOM / PID 额度 / CPU throttling / 存储拒绝 / stdout 截断；不同预算不能相互替代 |
| PostgreSQL 事务、SKIP LOCKED、Worker 行锁、清理后释放容量 | `make demo SCENARIO=capacity` | 竞争领取唯一归属；容量 2、第三个等待；结果提交仍占名额；清理失败保留预留并停止接单 |
| HTTPS 重连管理原 Sandbox、幂等重报、恢复窗口到期失败 | `SCENARIO=recovery`、`SCENARIO=expiry` | 保留原 Execution / Sandbox；重报同一结果；到期 `worker_lost`；不自动重跑，不承诺 Worker 进程重启续跑或跨节点迁移 |
| 77 项 Linux 验收 | `make demo-verify` 中的 T15 日志 | 77 是历史版本的顶层测试计数；本次以 `execution.log` 与 `creation.log` 的实际 PASS 数为准，不把子测试、普通测试和 Worker 测试混加 |

## 面试前准备一次

在 Linux 文件系统里的仓库 checkout 运行，例如 `~/dev/build-your-own-docker`，避免从 Windows/VMware 共享文件夹直接运行，以减少权限、可执行位和文件系统差异。准备符合 `go.mod` 的 Go 工具链，以及 `bash`、`make`、`python3`、CA 证书、util-linux、iproute2、sudo。推荐给虚拟机 4 GiB 内存、2 vCPU 和至少 4 GiB 可用磁盘，这是本地复现的实用配置，不是性能指标。

先在普通用户终端准备锁定输入和所有演示二进制：

```bash
make demo-prepare
```

此步骤可以联网，不需要 sudo。它从 `profile.lock.json` 获取输入，复用大小和 SHA-256 正确的缓存；下载到独立 `.part`，校验后原子替换。接着下载 Go 模块，编译当前 checkout，构建 Bundle，记录源码和全部产物摘要。只有全部成功才更新 `.cache/interview-builds/current`；失败不会把半成品标记为可用。

准备成功后，现场 `make demo` 不再下载依赖或重新编译。源码、锁文件、脚本或二进制变化会要求重新 `make demo-prepare`。构建清单用于发现意外混用旧产物，不能作为抵抗本机可信管理员篡改的签名。

单独检查来源缓存可用：

```bash
python3 tests/prepare-profile-sources.py --check
```

`PROFILE_SOURCES_READY` 表示所有锁定归档都通过检查。缓存检查不等于内核验收。

然后刷新 sudo 并检查当前内核能力：

```bash
sudo -v && make demo-doctor
```

预检验证 x86_64 Linux、Go、工具、mount/PID/network/user namespace 以及 cgroup v2 下 `cpu memory pids` 已委派、可创建清理子 cgroup、存在 `cgroup.kill`。它不调整父 cgroup 的 controller 配置。如果自动发现的父目录没有委派，指定已配置好的目录后重试：

```bash
export SANDBOX_TEST_CGROUP_PARENT=/absolute/delegated/cgroup
sudo -v && make demo-doctor
```

不要把示例路径原样执行。不能通过预检时先修复 VM；不要忽略报错继续展示。

## 准备专用测试 PostgreSQL

Worker 演示会同时创建临时测试角色和数据库，并将数据库交给独立的非超级用户角色。请使用专用测试实例的管理员账号；仅有 `CREATEDB` 权限不足以运行这些用例。禁止指向已有业务数据库。CI 使用 PostgreSQL 17；本地有专用 17 实例时直接配置连接即可。

如果 VM 已安装 Docker，可提前拉取一个仅用于测试数据库的容器。Docker 在这里承载 PostgreSQL；Python 隔离仍由本仓库自己实现：

```bash
docker pull postgres:17
docker run -d --name sandbox-interview-postgres \
  -p 127.0.0.1:55432:5432 \
  -e POSTGRES_PASSWORD=interview-local-only \
  --tmpfs /var/lib/postgresql/data:rw \
  postgres:17
```

这里的密码仅为可丢弃本地测试实例。已经创建容器时不要再次 `docker run`，使用 `docker start sandbox-interview-postgres`；端口冲突时选择另一端口，并同步修改下面的连接地址。等就绪检查返回 accepting connections：

```bash
docker exec sandbox-interview-postgres pg_isready -U postgres
export AGENT_TEST_DATABASE_URL='postgres://postgres:interview-local-only@127.0.0.1:55432/postgres?sslmode=disable'
```

Worker 与 Control Plane 的传输测试使用真实 HTTPS 和私有 CA。上述 `sslmode=disable` 仅作用于 VM loopback 上的测试 PostgreSQL。演示日志不打印连接串、Worker Credential 或私钥。容器里的 `pg_isready` 只说明数据库开始监听；涉及数据库的演示和整体验收还会从 VM 宿主运行有 5 秒期限的只读 `agentctl migrate --status`，先发现宿主访问规则、端口或凭据错误，再进入正式场景。

完成全部预演：

```bash
sudo -v && make demo-verify
```

必须以普通用户运行：常规协议测试需要验证非 root 边界，内核 runner 自行通过 `sudo -n` 提权。这个入口依次执行 `make check`、数据库迁移和 Worker API 验收、完整 T15、完整 Worker Linux 验收；其中任一步失败都没有 `INTERVIEW_VERIFY_PASS`。测试工具会临时创建数据、namespace 和 cgroup，并按各自合同清理。能力预检成功不能替代此步骤。

`demo-verify` 会重新进行 Go 静态检查、编译和测试。它禁止自动下载模块与工具链，须保留 `demo-prepare` 使用的同一用户的 Go 模块缓存和本地工具链；缓存缺失会直接失败。现场 `make demo` 使用准备好的产物。

## 现场按顺序讲解

运行时原理可先单独展示：`sudo -v && make demo SCENARIO=runtime`。它使用已校验的 Alpine 缓存，在外层隔离 mount/PID/network namespace 和独立 cgroup 子树内运行，不占用宿主已有 bridge 或 `/sys/fs/cgroup/mydocker`。观察容器 PID 1、到 bridge 的 ping、只写 OverlayFS upper、退出 7 传回调用者、新建 ID 不同、按 ID 恢复旧文件。保留 `.cache/profile-sources` 或原 `PROFILE_BUNDLE_SOURCE_CACHE`，这个场景需要锁定归档。

先准备一个与预演相同的终端，确认 PostgreSQL 已就绪并设置好 `AGENT_TEST_DATABASE_URL`。展示单个场景比直接滚动全部测试更容易解释：

```bash
sudo -v && make demo SCENARIO=files
```

先讲输入 `sales.csv` → Python → 显式声明 `summary.json` → 有界快照与 SHA-256。再指出：修改只读输入失败后，原 Sandbox 仍可合法执行；路径穿越和不可信来源在创建前拒绝。

```bash
sudo -v && make demo SCENARIO=lifecycle
```

对照 Init PID、连续执行留下的文件、超时终止原因和 cgroup 清理。解释「Sandbox 保留上下文；Execution 是每次有界执行」，以及为什么先回收后代才提取输出和复用。

```bash
sudo -v && make demo SCENARIO=worker
sudo -v && make demo SCENARIO=capacity
```

解释两把锁：Execution 行的 `SKIP LOCKED` 防重复领取，Worker 行锁保护同一身份的总容量。结果完成、Sandbox 清理、释放容量是三件不同的事，不能在结果到达时提前释放。

```bash
sudo -v && make demo SCENARIO=recovery
sudo -v && make demo SCENARIO=expiry
```

恢复场景注入执行中断连、结果待上报、无响应 HTTPS 和 ACK 丢失，检查执行身份与不可变结果。结果待上报场景在首个 `/complete` 请求处注入故障，确保 Supervisor 已保留执行结果；查看 `result_pending=true` 以及恢复前后相同的 Sandbox 标识。故障注入中的传输错误、超时日志可能是预期现象；判据是断言和最终成功标记。解释「重试结果报告」为什么不会变成「重新运行结果不明的 Python」。

面试官追问隔离或预算时再运行：

```bash
sudo -v && make demo SCENARIO=security
sudo -v && make demo SCENARIO=resources
```

完整串行演示为 `sudo -v && make demo`。它要求数据库，并运行 Runtime Lab、Sandbox 与 Worker 场景；不要用它的通过替代 `demo-verify`。资源耗尽和恢复等待有真实耗时，应以自己的 VM 预演时间安排，不声称固定几分钟。

## 留存证据与定位失败

每次运行生成新的 `.cache/interview-runs/run-*`，包含场景日志、环境信息、构建清单与最终 `result.log`。底层 Sandbox 和 Worker runner 也保存独立日志。文件夹是本地证据，不提交 Git。没有 `result.log` 中的成功标记就是未通过；部分 `DEMO` 行不能代表整组成功。

| 现象 | 下一步 |
| --- | --- |
| `sudo: a password is required` | 在当前终端重新 `sudo -v`，直接重跑；准备与演示已分开，不必重新编译 |
| Source / prepared artifact changed | 用当前 checkout 重跑 `make demo-prepare`，不要手改 manifest |
| 缺少归档 / SHA-256 不匹配 | 联网运行 `python3 tests/prepare-profile-sources.py`，成功后重新准备；只设置缓存目录不会下载 |
| cgroup controller 未委派 / namespace 不可用 | 修复专用 VM 的内核、权限或父 cgroup 配置；不要降级到跳过用例 |
| 数据库连接失败 | 检查 PostgreSQL 就绪和端口，确认当前终端变量及测试账号；不要打印密码排查 |
| 无匹配测试 / 必需用例缺失 / SKIP | 从当前代码重新准备；runner 拒绝把旧二进制、零测试或跳过算通过 |
| 清理失败 | 保留日志和占用现场，依据明确的 Sandbox/cgroup 身份排查；不要盲目递归清理宿主路径或手工归还容量 |

现场网络不可用时，已经准备好的二进制、Bundle、本地 PostgreSQL 和缓存仍可用于演示。VM 本身若发生故障，可以展示提前保存的日志并明确标为预演记录，不能当成当场执行。

## 讲解时守住这些边界

这是共享 Linux 内核的原型，未证明任意恶意代码下的生产级安全。Supervisor 的顺序执行已经可复现，当前 Worker 调度仍为每个 Agent Run 一次 Execution；没有完整 Operator Console、上传下载流程或自动 Agent Loop。恢复只覆盖同 Worker 进程仍在、原 Sandbox 尚存活的有界 HTTPS 故障，不是 CRIU、跨节点迁移或 exactly-once 副作用承诺。

代码阅读顺序：`CONTEXT.md` → `internal/sandboxsupervisor/protocol.go` → `execution_linux.go` / `extraction_linux.go` → `internal/executionqueue/store.go` / `capacity.go` / `recovery.go` → `internal/worker/worker_linux.go`。更详细的设计取舍见 [T13/T07](learning/t13-t07-sandbox-boundary.md)、[T14](learning/t14-declared-output-extraction.md)、[T27/T31](learning/t27-t31-worker-capacity-recovery.md)。
