# Worker API v1

T22 / #28、T23 / #29、T24 / #30 建立 Worker 身份、持久 Execution 领取和单次沙箱执行链路。Control Plane 访问 PostgreSQL；Worker 只持有自己的凭据，通过校验证书的 HTTPS 调用 Worker API，再通过本地 Unix socket 请求 Sandbox Supervisor。

T27 / #33 增加持久容量预留与有界并发；T31 / #37 增加同 Worker、原 Sandbox 的心跳恢复和 `worker_lost` 终止。当前仍是一份 Agent Run 对应一次 Execution，不包含完整 Agent Loop 或 Operator Console。

## 启动与凭据

先配置 Control Plane 的 `AGENT_DATABASE_URL` 并运行 `agentctl migrate`。启动要求迁移版本至少为 5；0004、0005 分别添加容量预留和恢复状态，原有已发布迁移内容不变。

```sh
agentctl worker provision --id worker-home --expires-at 2026-12-01T00:00:00Z
agentctl worker list
agentctl worker rotate --id worker-home --expires-at 2027-01-01T00:00:00Z
agentctl worker revoke --id worker-home
```

日期是示例，实际到期时间必须在未来。`provision` 和 `rotate` 的 JSON 输出含一次新 `token`；列表、撤销和错误不返回 token 或摘要。凭据有 256 位随机熵，数据库仅存 SHA-256 校验值。每个 Worker 的到期、撤销和轮换相互独立；轮换使旧凭据失效，重复撤销幂等。

将 token 写入专用 Worker 账户所有、仅该账户可读的普通文件，例如权限 `0600`；传给 Worker 的是文件路径。加载时检查已打开文件的所有者、类型和权限，拒绝符号链接、FIFO 等特殊文件。Worker 启动时加载一次凭据，轮换后需替换私有文件并重启 Worker。Worker 命令仅支持 Linux；Windows 不提供等价 Linux 权限验证。

```sh
control-plane --listen 127.0.0.1:8443 \
  --tls-cert /etc/agent/control-plane.crt --tls-key /etc/agent/control-plane.key \
  --lease-duration 2m --recovery-window 30s

# 在专用的非 root 账户下运行；先配置并启动本地 sandboxd。
worker --control-plane https://localhost:8443 \
  --ca-file /etc/agent/ca.crt --credential-file /etc/agent/worker.credential \
  --supervisor-socket /run/agent/sandboxd.sock \
  --profile-identity sha256:<installed-profile-digest> --capacity 2
```

使用系统信任的证书时可省略 `--ca-file`。Worker 拒绝 HTTP、不校验证书的 Transport 和重定向；启动环境不需要数据库、模型或对象存储凭据。当前 Control Plane 入口只接受 loopback IP，在监听前检查数据库、迁移和列类型，不自动迁移或公开 Operator 路由。跨机器生产入口和角色部署分别由 T48、T50 完成。

`--capacity` 为 1–64，默认 2；有占用时拒绝改变容量，相同配置幂等。并发 2 个 Sandbox 还需要为 Supervisor 预留至少 131072 个 subordinate UID/GID（`--subid-count 131072`）以及相应的 cgroup、内存和结果暂存预算。容量数字不会自动扩容这些底层资源。

### 从旧执行协议升级

旧版本没有持久记录 Sandbox ID，不能把旧记录的空 ID 当作“没有创建过 Sandbox”。迁移将此类历史占用标为 `cleanup_unknown`，Worker 停止接单，普通 Release 拒绝释放。升级时先撤销对应 Worker 凭据，停止旧 Worker，确认其 Supervisor 下的旧 Sandbox、进程和 cgroup 都已清理，再执行：

```sh
agentctl worker confirm-legacy-cleanup --id worker-home
```

此命令只接受已撤销的 Worker，是 Platform Operator 对既有本地清理事实的显式确认，不执行远程清理，也不证明未知资源已经消失。它只解除旧版本的未知预留，保留完成结果，将未完成旧执行终止为 `worker_lost`。确认后可用 `agentctl worker rotate` 签发新凭据并启动新版 Worker；重复确认幂等。新的创建前绑定使后续未绑定租约可以明确识别为尚未创建 Sandbox。

## HTTP 合同

所有操作使用 `POST` 和 `Authorization: Bearer <Worker Credential>`。Worker 身份从凭据解析，请求不能指定其他 Worker、数据库地址、Profile 或宿主机路径。

| 路径 | 请求 | 成功响应 |
|---|---|---|
| `/worker/v1/claim` | `{"wait_ms":20000}`，范围 0–25000 ms | `200` 返回 Lease，无可领任务返回 `204` |
| `/worker/v1/validate` | `execution_id`、`generation` | 当前 Worker 拥有有效 Lease 时返回 `204` |
| `/worker/v1/complete` | `execution_id`、`generation`、`result` | 首次提交或相同结果重报返回 `204` |
| `/worker/v1/configure-capacity` | `capacity` | 设置本 Worker 的可信启动容量，返回 `204` |
| `/worker/v1/capacity` | `{}` | `200` 返回 `worker_id`、`configured`、`occupied`、`available` |
| `/worker/v1/release` | `execution_id`、`generation` | 确认本地清理完成，幂等释放名额，返回 `204` |
| `/worker/v1/bind-sandbox` | `execution_id`、`generation`、`sandbox_id` | 创建前持久绑定原 Sandbox，返回 `204` |
| `/worker/v1/heartbeat` | 同上 | 活租约续期，`200` 返回 Authority |
| `/worker/v1/recover` | 同上 | 窗口内恢复原绑定，`200` 返回 Authority |
| `/worker/v1/outstanding` | `{}` | `200` 返回本 Worker 最多 64 条未释放预留 |

Lease 包含 `execution_id`、`agent_run_id`、`generation`、数据库生成的 `expires_at`，以及有界 `source`、`stdin`、`output_paths`。Profile identity 来自 Worker 可信启动配置，Worker 自行产生不可预测的 Sandbox ID。

Authority 包含仅有身份与期限的 `lease`、`state`、`server_time`、`recovery_until`。Outstanding 条目包含同样的精简 `lease`、`sandbox_id`、`state` 和升级清理状态，没有可供重跑的 Workload。已完成结果的 heartbeat/recover 仅确认 `completed`，不续租、不修改结果。

错误状态为 `400` 非法协议或结果，`401` 无效、撤销或到期凭据，`409` 错误 Worker、代际、失效租约或冲突结果，`429` 本实例处理容量不足，`503` 数据库暂不可用。响应不带数据库错误详情或凭据，未知路由不能访问 Operator 功能。

小请求最多 1 KiB，拒绝未知字段、重复字段和多个 JSON 值。报告最多 162 MiB，以容纳 Supervisor 两个 8 MiB 输出流的最坏 JSON 转义及 32 MiB 文件的 base64 编码；这是编码上限，默认实际 stdout/stderr 配额仍各为 1 MiB。报告解码最多接受 16 个文件，并在读取第 17 个元素前拒绝；单次只处理一个报告。文件内容合计最多 32 MiB，必须与声明顺序、大小和 SHA-256 一致。提取失败须显式给出 `output_error`，不得发布部分文件。

HTTP 读头期限 5 秒，读写期限 35 秒，最多 64 个并发请求、每次处理最长 30 秒。每次轮询重新检查凭据，撤销会终止未拿到任务的长轮询；客户端默认单次请求期限 35 秒。

客户端最多读取 64 KiB 的成功响应。心跳等 JSON 响应的传输截断或响应体超时可重试；完整非法 JSON、错误身份、未知字段和超限响应不能通过重试分类绕过验证。报告链路自身发现断连时会主动触发心跳检查，不必等下一次周期心跳超时；只有报告接口失败而心跳仍健康时，报告预算继续消耗。

心跳确认 `completed` 后停止续租，此后等待该结果的精确 ACK 一律消耗报告预算。即使确认持续丢失也会有界清理、释放容量；已经持久化的结果不被改写或重新执行。

## 事务与失败语义

可信的 `executionqueue.Store.Enqueue` 是后续 Tool Call 持久化入口和测试设置边界，不在 Worker listener 暴露。当前 tracer 限定一个 Agent Run 对应一个 Execution；重复 ID 或 Run 返回冲突，T28 负责多步执行和 Sandbox 驻留。

领取事务先锁 Worker 行并验证凭据，检查未释放预留数量低于配置容量，再通过 `FOR UPDATE SKIP LOCKED` 选择 `queued` Execution，原子写入 Worker、租约代际、期限和容量占用。队列为空或没有名额时均返回 `204`，不会先领取再等待本地资源。

租约默认 2 分钟，恢复窗口默认 30 秒，两者均支持可信配置 100 ms–1 h。数据库时间裁决权限；Worker 用服务端时间差和本地请求开始时刻安排单调计时的 watchdog，避免时钟偏差或网络延迟延长授权。正常心跳间隔约为租约长度的三分之一，最长 10 秒；失败重试使用 Worker 的 `--retry-interval`。

普通心跳不能恢复过期租约。Worker 先通过只读 `InspectSandbox` 核对原 Sandbox，再用同 Worker、当前代际和同一绑定调用 Recover。没有发生所有权转移，所以恢复保留代际。失联时已有本地执行可在恢复窗口内继续；每次 Execution 的 Supervisor 执行期限仍独立生效。

Control Plane 的后台扫描每次最多推进 256 条过期记录，持久化 `recovering` 和 `worker_lost`，不依赖 Worker 轮询。失败重试、扫描和进程重启不延长原窗口；数据库暂不可用时保留原期限重试。已丢失 Execution 永不回到队列。

提交在同一事务内验证 Worker、代际、期限并锁定 Execution。首次完成后结果不可变；相同结果重报只确认原记录，不同结果返回冲突。已经接受的相同结果在租约到期后仍能确认，但凭据必须有效，Worker 和代际必须相同。

Workload 和结果保存为规范化 JSON 的 `bytea`，保留合法 NUL 输入输出。重报先解码为固定类型再编码，JSON 空白和键顺序不影响身份。

| 故障 | 当前行为 |
|---|---|
| 领取已提交，但响应丢失 | 保留占用，不重派；期限收敛为 `worker_lost` 后，可通过终态库存确认清理并释放 |
| Supervisor 执行请求发出后失联 | 报告执行结果不确定，清理并停止，不重新执行 |
| 结果提交失败或确认丢失 | 重报同一 Lease 和结果；健康连接下受报告预算限制，断连等待受恢复 watchdog 限制 |
| 错误 Worker、代际或首次报告已过期 | 拒绝结果，不覆盖持久状态 |
| Worker 被撤销 | 后续 API 操作拒绝；已发出的本地执行不承诺立即停止 |
| HTTPS 断连、租约到期 | 保留原本地执行连接与 Sandbox，进入 `recovering`；确认原 Sandbox 后可在窗口内恢复 |
| 恢复窗口耗尽 | 本地 watchdog 取消执行并清理，持久状态为 `worker_lost`；拒绝迟到结果，不自动重跑 |
| Worker 进程退出、本地 RPC 丢失 | 沿 Supervisor 放弃执行的取消／清理合同处理，不承诺跨进程续跑 |
| Sandbox 清理失败 | 保留容量并停止 Worker；修复后仍须确认 Destroy 成功再 Release |

Worker 使用有界并发槽，每个 Agent Run 使用新 Sandbox。Complete 不释放名额；独立清理期限内 Destroy 成功后才发送 Release。Release 对未完成执行同时确认 `worker_lost`，防止已销毁的 Sandbox 再获得执行权。清理失败的 Node 保持停止，即使稍后外部修复了容量也不会自行恢复接单。

每轮领取前，Worker 可从 Outstanding 清理 `completed`／`worker_lost` 的旧预留；不会触碰其他进程仍持有的 `leased`／`recovering`，也不会从库存重发 Python。默认长轮询 20 秒、重试间隔 250 ms、健康报告预算 30 秒、独立清理期限 10 秒，均由可信启动参数配置。撤销凭据可能使清理确认无法上报，此时数据库保守保留占用；恢复可信凭据后通过同 Worker 的终态清理路径处理。

## 验收

```sh
# 指向专用 PostgreSQL，测试只创建/删除自己拥有的临时数据库和角色。
make test-worker-api
# 专用 Linux、真实 Profile 源码缓存、sudo、cgroup v2 委派。
PROFILE_BUNDLE_SOURCE_CACHE=/absolute/profile-sources make test-worker-execution
```

两条入口均要求 `AGENT_TEST_DATABASE_URL`。普通 `go test ./...` 未配置数据库时显式跳过数据库用例。Linux 联合入口运行真实 PostgreSQL、TLS、Supervisor 和 CPython，保留独立日志，并拒绝 SKIP、零测试、缺必需用例及进程失败。CI 同时运行数据库协议验收与 Linux 联合验收。
