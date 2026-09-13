# Worker API v1

T22 / #28、T23 / #29、T24 / #30 建立 Worker 身份、持久 Execution 领取和单次沙箱执行链路。Control Plane 访问 PostgreSQL；Worker 只持有自己的凭据，通过校验证书的 HTTPS 调用 Worker API，再通过本地 Unix socket 请求 Sandbox Supervisor。

## 启动与凭据

先配置 Control Plane 的 `AGENT_DATABASE_URL` 并运行 `agentctl migrate`。新增迁移分别建立 Worker 身份与 Execution 队列；原有迁移内容不变。

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
  --lease-duration 2m

# 在专用的非 root 账户下运行；先配置并启动本地 sandboxd。
worker --control-plane https://localhost:8443 \
  --ca-file /etc/agent/ca.crt --credential-file /etc/agent/worker.credential \
  --supervisor-socket /run/agent/sandboxd.sock \
  --profile-identity sha256:<installed-profile-digest>
```

使用系统信任的证书时可省略 `--ca-file`。Worker 拒绝 HTTP、不校验证书的 Transport 和重定向；启动环境不需要数据库、模型或对象存储凭据。当前 Control Plane 入口只接受 loopback IP，在监听前检查数据库、迁移和列类型，不自动迁移或公开 Operator 路由。跨机器生产入口和角色部署分别由 T48、T50 完成。

## HTTP 合同

所有操作使用 `POST` 和 `Authorization: Bearer <Worker Credential>`。Worker 身份从凭据解析，请求不能指定其他 Worker、数据库地址、Profile 或宿主机路径。

| 路径 | 请求 | 成功响应 |
|---|---|---|
| `/worker/v1/claim` | `{"wait_ms":20000}`，范围 0–25000 ms | `200` 返回 Lease，无可领任务返回 `204` |
| `/worker/v1/validate` | `execution_id`、`generation` | 当前 Worker 拥有有效 Lease 时返回 `204` |
| `/worker/v1/complete` | `execution_id`、`generation`、`result` | 首次提交或相同结果重报返回 `204` |

Lease 包含 `execution_id`、`agent_run_id`、`generation`、数据库生成的 `expires_at`，以及有界 `source`、`stdin`、`output_paths`。Profile identity 来自 Worker 可信启动配置，Worker 自行产生不可预测的 Sandbox ID。

错误状态为 `400` 非法协议或结果，`401` 无效、撤销或到期凭据，`409` 错误 Worker、代际、失效租约或冲突结果，`429` 本实例处理容量不足，`503` 数据库暂不可用。响应不带数据库错误详情或凭据，未知路由不能访问 Operator 功能。

小请求最多 1 KiB，拒绝未知字段、重复字段和多个 JSON 值。报告最多 162 MiB，以容纳 Supervisor 两个 8 MiB 输出流的最坏 JSON 转义及 32 MiB 文件的 base64 编码；这是编码上限，默认实际 stdout/stderr 配额仍各为 1 MiB。报告解码最多接受 16 个文件，并在读取第 17 个元素前拒绝；单次只处理一个报告。文件内容合计最多 32 MiB，必须与声明顺序、大小和 SHA-256 一致。提取失败须显式给出 `output_error`，不得发布部分文件。

HTTP 读头期限 5 秒，读写期限 35 秒，最多 64 个并发请求、每次处理最长 30 秒。每次轮询重新检查凭据，撤销会终止未拿到任务的长轮询；客户端默认单次请求期限 35 秒。

## 事务与失败语义

可信的 `executionqueue.Store.Enqueue` 是后续 Tool Call 持久化入口和测试设置边界，不在 Worker listener 暴露。当前 tracer 限定一个 Agent Run 对应一个 Execution；重复 ID 或 Run 返回冲突，T28 负责多步执行和 Sandbox 驻留。

领取事务先锁 Worker 行并验证凭据，再通过 `FOR UPDATE SKIP LOCKED` 选择 `queued` Execution，原子写入 Worker、租约代际和期限。默认租约 2 分钟，可信配置范围 100 ms–1 h，应留足创建、执行和报告时间。期限以数据库时间为准，Worker 本机时间只用于保守截止本地执行。

提交在同一事务内验证 Worker、代际、期限并锁定 Execution。首次完成后结果不可变；相同结果重报只确认原记录，不同结果返回冲突。已经接受的相同结果在租约到期后仍能确认，但凭据必须有效，Worker 和代际必须相同。

Workload 和结果保存为规范化 JSON 的 `bytea`，保留合法 NUL 输入输出。重报先解码为固定类型再编码，JSON 空白和键顺序不影响身份。

| 故障 | 当前行为 |
|---|---|
| 领取已提交，但响应丢失 | 保持已租出，不自动重新领取或改派 |
| Supervisor 执行请求发出后失联 | 报告执行结果不确定，清理并停止，不重新执行 |
| 结果提交失败或确认丢失 | 当前进程在报告期限内重报同一 Lease 和结果 |
| 错误 Worker、代际或首次报告已过期 | 拒绝结果，不覆盖持久状态 |
| Worker 被撤销 | 后续 API 操作拒绝；已发出的本地执行不承诺立即停止 |
| Worker 停止或租约到期 | 不重新排队，读取可见 `lease_expired`；T31 负责恢复与 `worker_lost` |
| Sandbox 清理失败 | 返回清理错误并停止 Worker |

Worker 当前串行处理，每次使用新 Sandbox。成功确认或失败退出都用独立期限调用 DestroySandbox；报告重试期间保留原结果和 Sandbox。默认长轮询 20 秒、重试间隔 250 ms、报告总期限 30 秒、清理期限 10 秒，均可通过可信启动参数配置。没有心跳续租、跨进程结果恢复或自动改派。

## 验收

```sh
# 指向专用 PostgreSQL，测试只创建/删除自己拥有的临时数据库和角色。
make test-worker-api
# 专用 Linux、真实 Profile 源码缓存、sudo、cgroup v2 委派。
PROFILE_BUNDLE_SOURCE_CACHE=/absolute/profile-sources make test-worker-execution
```

两条入口均要求 `AGENT_TEST_DATABASE_URL`。普通 `go test ./...` 未配置数据库时显式跳过数据库用例。Linux 联合入口运行真实 PostgreSQL、TLS、Supervisor 和 CPython，保留独立日志，并拒绝 SKIP、零测试、缺必需用例及进程失败。CI 同时运行数据库协议验收与 Linux 联合验收。
