# T13 / T07：只读输入与 Sandbox 内核视图

我在 T14 中已经实现了声明输出的安全提取。这次补上真实文件输入，并收紧挂载、设备、描述符和网络边界，让一次执行可以完整展示“读入 CSV、处理、返回声明结果”。对应 [T13 / #19](https://github.com/TH1NKING/Build-your-own-Docker-with-Go/issues/19) 和 [T07 / #13](https://github.com/TH1NKING/Build-your-own-Docker-with-Go/issues/13)。

## 从一次请求读代码

1. [protocol.go](../../internal/sandboxsupervisor/protocol.go) 的 `CreateSandboxRequest.Attachments` 表示创建时固定的输入集合。每个元素只有 `staging_id` 与 `name`，没有宿主路径。
2. [server_linux.go](../../internal/sandboxsupervisor/server_linux.go) 严格解码整个请求；[attachments_linux.go](../../internal/sandboxsupervisor/attachments_linux.go) 也严格解码每一个嵌套元素，拒绝未知字段、重复字段和 `null` 元素。
3. `resolveAttachments` 在启动时固定的 `AttachmentRoot` 下打开标识对应的文件，检查类型、所有者、权限、硬链接数、大小和整批预算。全部通过后才进入创建流程。
4. [creation_linux.go](../../internal/sandboxsupervisor/creation_linux.go) 把已打开文件作为私有 FD 传给 bootstrap。请求只决定哪些已暂存输入可见，不能更改资源或隔离策略。
5. [bootstrap_linux.go](../../internal/sandboxsupervisor/bootstrap_linux.go) 在独立 namespace 中建立只读 input 挂载、设备和 proc 视图，再 `pivot_root` 并脱离旧根。附件 FD 在执行 Sandbox Init 前关闭。
6. Python 以内部 UID/GID 1000 读取 `/workspace/input/sales.csv`，在 `/workspace/output` 写结果。T14 继续在 Execution 后代终止、回收之后提取声明输出。

```json
{
  "schema": "sandbox-supervisor-request/v1",
  "request_id": "create-example",
  "operation": "create_sandbox",
  "parameters": {
    "sandbox_id": "example-run",
    "profile_identity": "sha256:<installed-profile-digest>",
    "attachments": [{"staging_id": "csv-001", "name": "sales.csv"}]
  }
}
```

这里的摘要占位符要替换为实际安装的 Profile identity。省略 `attachments` 的原有调用保持可用。

## 暂存区是谁的责任

`sandboxd --attachment-root /absolute/trusted/staging` 是可信启动配置；不配置时，有附件的请求返回 `operation_unavailable`。暂存区由可信组件或 Platform Operator 准备，Worker 只传入已授权的暂存标识。T13 不实现 Conversation 授权、上传界面或 ArtifactStore；这几项分别留在后续平台票据中。

暂存根必须由 Supervisor 有效 UID 所有，不能允许 group/other 写，且路径每一级都不能是符号链接。生产文件必须是 root-owned、精确 `0444` 的普通文件、只有一个硬链接。固定输入上限为每份 20 MiB、每个 Sandbox 合计 100 MiB、最多 16 份。输入名字仅允许 ASCII 字母、数字、点、下划线、连字符，长度 1–128，且不能是 `.` 或 `..`；不支持嵌套输入路径。

暂存目录和祖先还必须允许 subordinate UID 遍历，例如可信根下的 `0755` 目录。bootstrap 不会为了成功创建而修改宿主目录权限。不可遍历的 `0700` 祖先会使创建失败并清理；不能把这种环境限制误解成附件内容或权限已被绕过。

可信暂存者必须在整个 Sandbox 生命周期内保持源文件内容不变。只读 bind 限制的是 Sandbox 的访问路径，不能阻止可信宿主 root 从别的路径修改同一 inode。需要强制隔绝宿主写者时，可以另做密封快照，但会改变存储、内存计费和复制成本；当前合同不把可信 root 当作攻击者。

## 为什么这样实现

| 选择 | 解决的问题 | 其他做法的代价或缺口 |
|---|---|---|
| 不透明 `staging_id` + 固定可信根 | 特权进程不会按 Workload 指定路径打开宿主文件 | 接受任意路径再清洗，容易混淆授权与字符串校验 |
| `O_PATH`、逐级 `O_NOFOLLOW`、`fstat` | 检查打开对象，且不会因为 FIFO 或设备而阻塞、激活它 | 先 `stat(path)` 后 `open(path)` 之间可能发生替换 |
| 文件只读 bind + 只读 input 目录 | 同时禁止改内容、删除、改名和创建新输入 | 仅把文件 chmod 为只读，无法完整约束可写父目录中的删除或替换 |
| 独立 input tmpfs | 输入槽位与 T10 的可写 Workspace 文件预算分开 | 把每个挂载槽位放进可写预算，会让同一预算随附件数变化 |
| 全部输入先验证，再分配 Sandbox | 坏输入不留下半成品或消耗 ID | 边挂载边接受下一份输入，会增加中途失败的可见状态和清理难度 |
| 每份非递归 bind，附加 `ro,nosuid,nodev,noexec` | 不带入源目录下的其他内容或子挂载，输入不成为设备/可执行文件入口 | 直接挂整个暂存目录会暴露未声明输入 |

还有一个容易忽略的 Linux 细节：在父 mount namespace 中打开的 FD，不能直接当成新 namespace 中可靠的 bind 来源。bootstrap 在覆盖宿主 `/tmp` 前，先根据继承 FD 找到源路径，在新 namespace 中逐级重新打开，并比较设备号、inode、模式、UID/GID、硬链接数、大小。新路径若指向另一对象就拒绝；后续 bind 使用已经检查过的新 FD。源路径始终属于私有 bootstrap，不出现在公开协议中。

## T07 收紧了什么

[hardening_linux.go](../../internal/sandboxsupervisor/hardening_linux.go) 建立独立、定额、只读的 `/dev`，仅暴露 `/dev/null` 和 `/dev/zero`。在绑定前使用 `O_PATH|O_NOFOLLOW` 与设备号检查，避免把整个宿主 `/dev` 带入沙箱。不提供 TTY、PTY 或任意块设备；Python 随机数继续走已允许的 `getrandom`。

“设备挂载只读”不意味着字符设备不能处理 `write`：`/dev/null` 本来就应当接受并丢弃写入。所以安全性依赖严格的设备白名单，不能只依赖 `ro` 标志。

`/proc` 是新 PID namespace 的独立、只读挂载，固定的敏感目录和文件使用只读空视图覆盖。Workload 仍能读取自身进程状态，但看不到宿主 PID 列表、宿主 `/sys` 或旧根。固定掩码不会把所有共享内核统计都变成私有数据；例如 `meminfo`、`uptime` 仍可能反映共享内核。

生产 Network Policy 始终为 `none`。验收分两层：通过公开 `ExecutePython` 检查网络 syscall 被拒绝，再由可信 Linux 测试探针在不安装 Workload seccomp 的情况下验证独立 network namespace 无双向连通性。这可以区分“syscall 被过滤”和“网络确实隔离”两种证据。Runtime Lab 的网络实验入口继续独立存在。

描述符测试向 Supervisor 注入宿主目录、普通文件、监听 socket、已连接 socket 和高编号 FD，再检查 Init 与 Python。白名单外的句柄不能靠继承穿过 `pivot_root` 留下另一条宿主访问路径。

## 安装与边界

Profile 安装布局现在预留空 `/dev`，实际设备只在每个 Sandbox 的私有 mount namespace 中建立。旧安装目录如果没有这个槽位，新 bootstrap 会拒绝创建；升级应重新构建 Bundle 并安装到新的可信 store，不能手工修改已有内容寻址目录或绕过摘要校验。

这一阶段完成的是受约束的本地 Sandbox 合同。它不提供多用户授权、完整 Worker 平台、跨重启恢复或对所有 Linux 内核漏洞的防护，也不证明可以对匿名 Internet 攻击者开放执行。

三组可重复操作见 [文件演示](sandbox-file-demo.md)，组合验证入口及实测记录见 [T15 联合验收](t15-kernel-acceptance.md)。
