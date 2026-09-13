# 三组 Sandbox 文件演示

这套脚本使用公开 Supervisor 协议、真实 Python Profile 和独立 Linux 内核 fixture。每次调用会安装临时 Profile、准备可信输入、启动 Supervisor、执行检查并清理 Sandbox。`DEMO` 行展示实际结果，任何断言失败都会返回非零状态。

## 准备一次

在专用、可丢弃的 Linux 环境中进入仓库根目录。需要满足 `go.mod` 的 Go 工具链、Bash、util-linux、iproute2、sudo，以及可委派 `cpu memory pids` 的 cgroup v2。下载阶段还需要 curl、CA 证书和 jq；Ubuntu 可以先安装辅助工具：

```bash
sudo apt update &&
sudo apt install -y curl ca-certificates jq util-linux iproute2
```

**运行演示前，必须先下载并校验 Python Profile 的两份输入归档。** `git clone` 不包含 `.cache`，Go 自动下载的模块也不包含这两份运行时文件。先指定当前 checkout 的缓存位置：

```bash
export PROFILE_BUNDLE_SOURCE_CACHE="$(pwd)/.cache/profile-sources"
```

这条 `export` **只指定目录，不会下载文件**。接着完整执行 [下载与校验命令](../profile-bundle-v1.md#prepare-the-source-cache)，直到输出 `PROFILE_SOURCES_READY`。命令从锁定文件读取 URL、文件名、精确大小与 SHA-256，复用已校验的缓存，补齐缺失或损坏的文件。当前目录应包含：

```text
.cache/profile-sources/
├── alpine-minirootfs-3.22.5-x86_64.tar.gz
└── cpython-3.14.7+20260825-x86_64-unknown-linux-musl-install_only_stripped.tar.gz
```

无需手工解压；构建工具读取这些归档并组装 Bundle。换一个 clone 目录时，需要重新准备缓存，或把 `PROFILE_BUNDLE_SOURCE_CACHE` 指向先前已校验的绝对路径。

缓存准备完成后，再按环境选择 cgroup 父目录并启用首次构建：

```bash
# 如果自动发现的 cgroup 父目录不可用，指定已委派的测试目录：
# export SANDBOX_TEST_CGROUP_PARENT=/absolute/delegated/cgroup
unset SANDBOX_TEST_BIN_DIR
```

第一次运行会构建命令和 Bundle，可能下载 Go 模块。下面的 `sudo -v` 先验证密码，供脚本稍后的 `sudo -n` 使用，无需配置永久免密 sudo。只在构建和演示成功后设置 `SANDBOX_TEST_BIN_DIR` 复用产物；代码改变后取消这个变量，让脚本重新构建。

如果看到 `profile-bundle: open locked source ... no such file or directory`，说明归档没有位于当前缓存目录，尚未进入 Sandbox 执行。检查 `printf '%s\n' "$PROFILE_BUNDLE_SOURCE_CACHE"` 和 `ls -lh "$PROFILE_BUNDLE_SOURCE_CACHE"`，再执行上面的下载与校验命令；只留下 `.part` 表示下载或校验没有完成。

## 1. 正常处理：文件确实进入并离开沙箱

```bash
sudo -v &&
bash tests/run-sandbox-file-demo-linux.sh normal &&
export SANDBOX_TEST_BIN_DIR="$(pwd)/.cache/sandbox-init"
```

输入 `sales.csv`：

```csv
item,amount
apple,12
pear,8
apple,5
```

Python 从 `/workspace/input/sales.csv` 读取三行数据，把 `12 + 8 + 5 = 25` 写入 `/workspace/output/summary.json`。请求只声明这个输出。

应看到：

```text
DEMO normal: rows=3 total=25; extracted summary.json = {"rows": 3, "total": 25}; size=24 SHA256=79694548e76ef2eef1839ab0932a19043073915132d018f6c1d259de65790948
PASS
```

关注三个不同事实：stdout 只报告处理进度；结果 JSON 来自实际提取的文件字节；大小和 SHA-256 对应那份字节快照。这里还没有 ArtifactStore 上传或下载界面。

如果首次构建耗时过长，末尾出现 `sudo: a password is required`，而 `.cache/sandbox-init/python.bundle` 已成功生成，可以重新运行 `sudo -v`，再用 `SANDBOX_TEST_BIN_DIR="$PWD/.cache/sandbox-init" bash tests/run-sandbox-file-demo-linux.sh normal` 复用已构建产物。尚未生成 Bundle 时应保持 `SANDBOX_TEST_BIN_DIR` 未设置，补齐来源或修复构建错误后重试。

## 2. 破坏输入：拒绝后仍可继续执行

```bash
sudo -v && bash tests/run-sandbox-file-demo-linux.sh read-only
```

脚本依次尝试覆盖输入、删除输入、把输入移到 output、在 input 新建文件、修改文件权限、修改 input 目录权限。它要求六次都被拒绝，并读取 `/proc/self/mountinfo` 确认文件与 input 目录确实带有 `ro,nosuid,nodev,noexec`。

应看到：

```text
DEMO read-only: six mutations denied; read-only mounts verified
DEMO read-only: same Sandbox executed again; trusted input unchanged
PASS
```

最后一个结果同样重要：拒绝一次非法操作后，同一 Sandbox 仍能读原始输入并写入合法 output。销毁后，可信暂存源仍完整保留。

## 3. 恶意来源：在创建之前拒绝

```bash
sudo -v && bash tests/run-sandbox-file-demo-linux.sh malicious
```

脚本提交不存在的标识、符号链接、目录、FIFO、硬链接、可写文件、错误所有者、超大文件、`../valid` 和 `/etc/passwd`。每种来源都必须得到 `invalid_reference`。还会检查嵌套 JSON 的未知字段、重复字段、非法名字与重复声明。

典型输出：

```text
DEMO malicious: "symlink" -> invalid_reference; corrected request reused the unconsumed Sandbox ID
DEMO malicious: "../valid" -> invalid_reference; corrected request reused the unconsumed Sandbox ID
DEMO malicious: "/etc/passwd" -> invalid_reference; corrected request reused the unconsumed Sandbox ID
PASS
```

每次拒绝后，脚本都会改用合法输入，以相同 Sandbox ID 成功创建再销毁。这样验证拒绝路径没有消耗 ID 或留下需要人工处理的半成品。FIFO 用例也要求请求及时返回，避免特权进程在打开特殊文件时挂起。

## 连续演示与完整验收

```bash
sudo -v && bash tests/run-sandbox-file-demo-linux.sh all
unset SANDBOX_TEST_BIN_DIR
sudo -v && bash tests/run-sandbox-acceptance-linux.sh
```

三组演示适合向别人解释正常路径和失败行为；完整验收还检查身份映射、旧根、内核视图、网络、seccomp、资源预算、超时、输出和清理。后者没有执行完并通过时，不能用演示通过代替 T15 结论。

读代码时从 [sandbox_attachment_linux_test.go](../../tests/sandbox_attachment_linux_test.go) 的三个演示测试开始，再跟到 [T13/T07 设计说明](t13-t07-sandbox-boundary.md)。
