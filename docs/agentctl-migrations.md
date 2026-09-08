# agentctl PostgreSQL 迁移

`agentctl migrate` 是 Platform Operator 显式执行数据库升级的运维入口。
本阶段以 PostgreSQL 17 为支持和 CI 验收基线，迁移由 Go 二进制随版本发布。
数据库保存 Control Plane 的持久状态；Worker Node 不连接数据库。

## 构建和连接

在仓库根目录构建：

```sh
go build -o agentctl ./cmd/agentctl
```

连接串必须通过 `AGENT_DATABASE_URL` 显式提供；未设置或解析失败时命令失败。
部署时由可信环境配置注入真实值，以下仅展示本地开发形式：

```sh
export AGENT_DATABASE_URL='postgres://operator:development-password@127.0.0.1:5432/agent?sslmode=disable'
./agentctl migrate
```

PowerShell 使用 `$env:AGENT_DATABASE_URL = '...'` 设置同名环境变量。
数据库应由 Platform Operator 预先创建，迁移账号需有建 schema、建表和执行迁移 SQL 的权限。
生产连接应遵循部署环境的 TLS 配置；示例的 `sslmode=disable` 仅用于本地开发。
连接解析及 PostgreSQL 协议由 [pgx](https://pkg.go.dev/github.com/jackc/pgx/v5) 提供。

## 命令合同

```text
agentctl migrate [--status] [--timeout 1m] [--migrations-dir DIRECTORY]
```

| 参数 | 行为 |
| --- | --- |
| 无参数 | 应用尚未提交的迁移，直至当前发布所含的最新版本 |
| `--status` | 只读报告数据库版本；空库为 0，不创建版本表或业务对象 |
| `--timeout` | 默认 `1m`；限制本次运行的连接、等待迁移锁和 SQL 执行时间 |
| `--migrations-dir` | 使用可信运维目录的完整迁移序列，替换默认内嵌 SQL |

成功时退出码为 0，标准输出为一行：

```text
schema_version=1 applied=1
```

`schema_version` 是当前数据库版本，`applied` 是本次成功应用的文件数量。
重复执行已经完成的迁移输出 `schema_version=1 applied=0`。
`--status` 的 `applied` 总是 0；空库输出 `schema_version=0 applied=0`。
超时覆盖整次运行，不会在每份文件开始时重新获得一整分钟。
取消时主动向 PostgreSQL 发送 CancelRequest；网络回退最多额外等待 2 秒，
回滚和关闭连接分别使用独立的 5 秒清理期限，避免使用已经取消的工作 context。
因此 `--timeout` 是工作期限，不承诺进程在该毫秒点立即退出。

## 迁移文件与版本记录

默认 SQL 放在 `internal/migrations/sql/`，通过 `go:embed` 编入 `agentctl`。
文件按连续递增的正整数编号，第一份为版本 1，例如：

```text
0001_control_plane.sql
0002_worker_credentials.sql
0003_execution_leases.sql
```

后两项仅展示将来的命名方式，不表示本 ticket 已创建这些业务对象。
本 ticket 的第一份迁移只执行 `CREATE SCHEMA control_plane;`。
具体业务表由后续 ticket 增量引入。

runner 在 `public.agentctl_schema_migrations` 保存成功版本、文件名及 SQL 的 SHA-256。
建立这张版本表属于迁移基础设施 bootstrap，不占用一个业务 schema 版本。
迁移编号必须从 1 连续递增；不能跳号、重复编号或漏掉历史文件。
已应用文件不可改名或修改内容，包括空白和换行；新增变更必须写下一版本。
重跑会核对历史记录，拒绝数据库与迁移来源不一致的情况。
`--migrations-dir` 必须包含完整历史，不能只放“这次新加”的文件。

## 并发和事务边界

一次迁移运行使用同一数据库连接持有 advisory lock，覆盖版本检查与所有待执行文件。
两个 `agentctl migrate` 同时启动时会串行执行，后取得锁的进程重新读取当前版本。
锁等待受 `--timeout` 约束，连接结束后会释放锁。
`--status` 也先等待同一个锁，再读取已提交版本；它不会校验本机 SQL 是否匹配历史。
这是迁移工具之间的协作约定；手工执行 SQL 的其他客户端也必须遵守运维流程。
参见 PostgreSQL 的 [advisory lock 语义](https://www.postgresql.org/docs/17/explicit-locking.html#ADVISORY-LOCKS)。

每份 SQL 和它的版本记录位于同一事务中，成功后再处理下一份文件。
若第 2 份文件执行失败，第 1 份已提交的变更保留；第 2 份全部回滚；第 3 份不会执行。
因此不会出现“表改了一半，但版本号已经前进”的正常 SQL 失败状态。
文件内每条语句成功并不等于该版本成功，必须等待整个事务提交。
参见 PostgreSQL 的 [事务说明](https://www.postgresql.org/docs/17/tutorial-transactions.html)。

## SQL 编写范围

只支持向前迁移以及能够放进事务的 SQL，不提供 down、强制跳版本或自动修复历史功能。
SQL 文件必须来自可信且经过 review 的发布内容；目录选项不是不可信 SQL 沙箱。
作者不得在文件中写控制外层事务的 `BEGIN`、`COMMIT`、`ROLLBACK` 等语句。
这项约束指事务控制语句，不禁止函数体中的 PL/pgSQL `BEGIN ... END` 语法。
不得写要求脱离事务的操作，如 `CREATE DATABASE`、`CREATE INDEX CONCURRENTLY`。
普通 `CREATE INDEX` 可以用于事务迁移，但仍需评估对现有业务访问的锁影响。
参见 [CREATE INDEX 的事务限制](https://www.postgresql.org/docs/17/sql-createindex.html)。
SQL 按完整文件交给 PostgreSQL，不自行按分号拆分，避免误拆函数体与字符串。

## 失败诊断与恢复

失败时命令返回非零退出码，并在标准错误说明失败阶段、相关迁移文件和已知当前版本。
有 PostgreSQL 错误码时输出 SQLSTATE，便于区分语法、权限或约束错误。
尚未读取版本的连接失败无法报告可靠版本，不应把未知版本当作空库版本 0。
命令不输出连接串、密码或驱动及数据库服务端的原始错误消息。
这会减少服务端诊断细节，但可避免数据库错误上下文把连接凭证或 SQL 内容带进日志。

普通 SQL 失败后，先查看报错文件和 SQLSTATE，再修复尚未成功应用的迁移并重新运行。
已经成功应用的文件应恢复为原始发布内容，修正动作通过新增迁移表达。
若提交期间连接中断，客户端可能无法确认服务端是否提交；重连运行 `--status` 核实版本。
不要根据超时或断线就手工推进版本表，也不要通过删除历史记录“解除”校验失败。

## 新增迁移和公开验收

1. 保留 `internal/migrations/sql/` 中所有历史文件，追加下一个连续编号的 SQL。
2. Review 事务兼容性、数据回填成本和锁影响，避免直接修改已发布迁移。
3. 在专用 PostgreSQL 17 测试实例运行公开 CLI 验收，再重新构建发布二进制。
4. 部署后显式执行 `agentctl migrate`，记录版本输出，重跑确认 `applied=0`。

集成测试使用 `AGENT_TEST_DATABASE_URL`，它与实际迁移使用的环境变量不同：

```sh
export AGENT_TEST_DATABASE_URL='postgres://postgres:test-password@127.0.0.1:5432/postgres?sslmode=disable'
go test ./tests -run TestAgentctlMigration -count=1
# CI 使用这个入口，缺少测试数据库配置时直接失败：
make test-migrations
```

该变量必须指向专用测试 PostgreSQL 管理员账号，不可使用生产数据库 URL。
每个测试创建自己的临时 database 和非超级用户 role，并在结束后删除它们。
测试通过真实 `agentctl` 进程及 PostgreSQL 验收，不以数据库 mock 推断事务是否回滚。
未设置测试变量时，需要真实数据库的用例会跳过；跳过不代表已经通过 PostgreSQL 验收。
验收重点包括空库初始化、重复运行、只读状态、失败回滚、历史一致性、并发与超时。

## 为什么选择这套方式

- 显式 SQL 便于 review 具体 DDL 与数据迁移；ORM 自动迁移更难表达和审查有顺序的数据回填。
- 每文件一个事务保留已完成进度；把整个升级包放在一个大事务里，会让后段失败回滚所有前段工作。
- SQL 与版本同事务提交，使两者保持一致；分别提交会留下变更已发生但版本未更新的中间状态。
- 内嵌 SQL 随二进制发布，部署无需另找匹配目录；默认外部目录容易漏文件或混入另一发布的迁移。
- 可选目录支持可信运维验证；只有顺序、向前、事务化迁移，保持当前需求的实现范围明确。
