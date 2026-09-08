# T10：为 Workspace 与临时存储建立内核强制预算

T09 已经限制了 Sandbox 的 CPU、内存、swap 和 PID；T12 与 T11 分别补上执行期限和有界输出。T10 解决文件系统里的资源消耗：Workload 即使没有突破隔离，也可能不断写大文件、创建小文件，或者让文件在多次 Execution 之间累积。

本文对应 [T10 / #16](https://github.com/TH1NKING/Build-your-own-Docker-with-Go/issues/16)，延续 [ADR-0013 的可写目录边界](../adr/0013-restrict-sandbox-writes-to-the-workspace.md) 和 [ADR-0014 的资源预算](../adr/0014-start-with-conservative-sandbox-resource-budgets.md)。领域里的约束叫 Resource Budget；`tmpfs`、`size` 和 `nr_inodes` 是实现它的 Linux 机制。

仓库之前已经为 `/workspace` 和 `/tmp` 挂载有限 tmpfs。这次的工作是把硬编码参数变成可信、可配置、语义明确的合同，并验证边界条件。不能把 T10 描述成“从零实现文件系统配额”，也不能仅凭它声称生产 Sandbox 的所有安全合同已经完成。

## 先说清楚限制什么

`ResourceBudget.Storage` 使用独立的 `StorageBudget` 类型。四个值都来自可信 Supervisor 启动配置，不放进公开执行请求。

| 启动参数 | 默认值 | 合同 |
| --- | --- | --- |
| `--workspace-bytes` | `536870912`，即 512 MiB | Workspace tmpfs 可以分配的数据容量 |
| `--workspace-files` | `5000` | Workload 在 Workspace 中可消耗的文件元数据名额 |
| `--temporary-bytes` | `16777216`，即 16 MiB | `/tmp` 所在独立 tmpfs 可以分配的数据容量 |
| `--temporary-files` | `1023` | Workload 在 `/tmp` 中可消耗的文件元数据名额 |

`--temporary-files=1023` 保留了旧配置的有效容量：以前使用 `nr_inodes=1024`，挂载根目录先消耗一个名额，剩余 1023 个。新的参数直接表达 Workload 可用的名额，避免把内核挂载参数和平台预算混为一谈。

两处文件系统的字节和文件额度独立。默认值意味着 Workspace 最多分配 512 MiB、`/tmp` 最多分配 16 MiB；不能将它们描述成“共同分享 512 MiB”。这些是上界，不是预先分配或保证可获得的容量。Sandbox 的内存预算和宿主可用资源仍可能先限制实际分配。

“文件元数据名额”比“普通文件数量”更准确。Linux tmpfs 会为目录、符号链接和额外硬链接计费；保留资源的已删除文件也可能继续占用。某种文件操作能否执行，还要先经过 System Call Policy。因此，参数为 5000 并不承诺无论目录结构如何都能再创建 5000 个普通文件。

## 可信配置怎样到达内核

配置传递可以按下面的顺序阅读：

```text
Platform Operator 启动 sandboxd
  → CLI 解析整数，写入 ResourceBudget.Storage
  → 服务启动前校验 StorageBudget
  → Supervisor 创建 Sandbox，以私有 bootstrap JSON 参数传递数值
  → bootstrap 严格解析并再次校验
  → 在私有 mount namespace 中挂载有限 tmpfs
  → exec 成为 Sandbox Init，之后才允许运行 Workload
```

公开的 `CreateSandbox`、`ExecutePython` 请求没有存储覆盖参数。Workload 改写自己的 `TMPDIR` 只会改变程序选择的文件路径，不会改变挂载预算；即使直接使用绝对路径或系统调用，也仍由目标文件系统执行限制。

“私有参数”表示它属于 Supervisor 与 bootstrap 的内部启动合同，不表示 JSON 是秘密。安全性来自调用边界、可信创建流程和在不可信代码开始前完成的约束，不来自隐藏参数名称。bootstrap 也不接受模型指定的宿主路径、挂载选项字符串或通用命令。

字节数必须是正数并按运行内核的页大小对齐，文件数必须是正数并满足计算上界。拒绝零值尤其重要：tmpfs 的 `size=0` 和 `nr_inodes=0` 会取消对应上限，而不是禁止分配。内核会向上取整 `size`，所以提前拒绝非页对齐值能避免实际容量悄悄大于 Operator 的配置。[Linux tmpfs 文档](https://cdn.kernel.org/doc/html/latest/filesystems/tmpfs.html)、[大小参数解析实现](https://github.com/torvalds/linux/blob/v6.8/mm/shmem.c#L3661-L3698)

文件数校验还为可信目录和内核元数据计费留下加法、乘法空间。它不仅防 Go 整数溢出，也避免生成内核不能接受的挂载参数。任一挂载失败时，创建沿既有失败清理路径结束；不能静默重试成没有额度的 tmpfs。

## 为什么 Workspace 要额外预留三个 inode

当前目录布局如下：

```text
只读 Runtime Profile 根文件系统
├── /workspace                 独立 tmpfs，根目录由可信身份拥有
│   ├── input                  可信身份拥有，0555，预留输入目录
│   └── output                 Workload UID/GID 1000 拥有，可写
└── /tmp                       另一个独立 tmpfs，Workload 可写
```

Workspace 挂载根、`input`、`output` 都会先消耗 inode，因此其挂载参数使用 `nr_inodes = WorkspaceFiles + 3`。`/tmp` 只需预留自己的挂载根，使用 `nr_inodes = TemporaryFiles + 1`。这些可信目录不能占掉平台承诺给 Workload 的名额。

T10 的 `/workspace/input` 是空的预留目录；T13 才负责真实 Attachment 的绑定。当前不可写性由可信所有者、`0555` 权限及不可写的父目录共同建立，不等于已经完成真实输入文件的只读绑定验收。Workload 不能把这个目录重新 chmod 为自己的可写空间。

保留目录与其内容来自同一个 tmpfs，挂载根本身也会经过内核的 inode 分配路径。预留数需要随可信目录布局维护；将来增设一个同文件系统的可信目录却忘记调整计费，会减少实际可用名额。[tmpfs 根目录创建](https://github.com/torvalds/linux/blob/v6.8/mm/shmem.c#L4108-L4116)

## 为什么同时使用 size 和 nr_inodes

字节额度和元数据额度防的是两种不同消耗。一个很大的文件消耗数据页；成千上万个空文件几乎不消耗文件数据，却仍需要 inode、目录项等元数据。只限制其中一个，不能表达完整的存储预算。

| 行为 | 主要消耗 | 需要的边界 |
| --- | --- | --- |
| 向一个文件持续写数据 | 文件数据页 | `size` |
| 创建大量空文件、目录、链接 | 文件系统元数据 | `nr_inodes` |
| Python 自身申请大数组 | 进程内存 | cgroup `memory.max` |
| 无限写满、删除，再写满 | 累计 CPU 和执行时间 | CPU 预算与 Execution 期限 |

这里限制的是**同时留存的存储量**，不是 Agent Run 一生累计写入了多少字节。删除并释放文件后重新写入，是预算允许的正常复用。若要限制累计写入量，需要另一个有明确计费口径的合同。

tmpfs 的 `size` 不是整个 Sandbox 的 RSS 上限，也不涵盖全部文件系统元数据。inode 名额提供另一个有限边界；T09 的 Sandbox 父 cgroup 则继续覆盖进程内存与被计费的文件数据、内核内存。

### 区分硬链接计费与 System Call Policy 拒绝

当前 `python-data-v1` 中，Python `os.link` 的调用路径会被 System Call Policy 拒绝并返回 `EPERM`。这不是 tmpfs 空间不足的证据，更不能将这个错误算作硬链接额度验收通过。

这里要保留一个重要区别：当前策略文件列有 `link`，没有开放 `linkat`。CPython 在具备 `linkat` 的平台上会选择这条调用路径；对应源码见 [CPython 3.14.7 的 `os.link`](https://github.com/python/cpython/blob/v3.14.7/Modules/posixmodule.c#L4148-L4159)。因此 `os.link` 返回 `EPERM` 不能推导为“所有硬链接系统调用均被禁止”；必须分清 Python 采用的系统调用与策略允许的操作。

项目的硬链接额度验收使用 `ctypes` 调用当前 Linux amd64 Profile 已经允许的 `SYS_link=86`，在额度耗尽时观察文件系统拒绝。这个调用仍经过相同的 seccomp 白名单，不需要增加 `linkat` 或改变 Profile 策略。它是限定到当前架构的验收路径，不是可直接复制到任意 CPU 架构的 Python 写法。独立机制实验则不安装该 Profile 策略，可以使用通常的 `os.link`。两类实验是否通过，需要各自的实际结果支持。

普通磁盘文件系统里，多个硬链接可以共享一个 inode。由此推断“`nr_inodes` 允许无限硬链接”并不适用于 Linux tmpfs：它会为已有文件新增的硬链接额外预留名额，因为新目录项也会占用内存。`O_TMPFILE` 首次链接是特殊情况，它使用创建匿名文件时已经扣除的额度。[tmpfs 硬链接实现](https://github.com/torvalds/linux/blob/v6.8/mm/shmem.c#L3113-L3147)

符号链接有自己的 inode，目录也一样。长符号链接还可能使用数据页，所以“还有 inode”不保证创建一定成功。扩展属性也可能消耗 tmpfs 元数据额度；当前 System Call Policy 没有开放任意扩展属性写入，不应为了凑足某个文件数结果而放宽它。

### unlink 为什么不一定马上归还额度

删除路径和释放文件不是同一个事件。如果进程仍持有文件描述符或映射，最后一个路径删除后，文件数据仍可能被使用。tmpfs 会在相应资源实际释放时归还额度，而不是只要目录里看不到文件就归零。[unlink 与回收实现](https://github.com/torvalds/linux/blob/v6.8/mm/shmem.c#L3148-L3163)、[inode 回收实现](https://github.com/torvalds/linux/blob/v6.8/mm/shmem.c#L1152-L1204)

因此可以构造一个很有价值的验收：创建文件、保持打开、删除路径，然后继续创建或写入；旧资源仍占额度，后续分配应受限。关闭最后一个引用后，再验证额度能够复用。这个场景也解释了为什么扫描目录树无法完整代表实际资源占用。

## 稀疏文件、mmap 与失败结果

`size` 限制已分配的数据页，不限制所有文件 `st_size` 的总和。`ftruncate` 可以把文件逻辑长度扩展到很大，但未分配的洞不需要对应数量的数据页。写入洞里的数据才会逐页消耗额度。不能把“大于 512 MiB 的逻辑文件创建成功”直接判为绕过。[tmpfs 对稀疏文件的计费说明](https://github.com/torvalds/linux/blob/v6.8/mm/shmem.c#L174-L178)

这也给后续 Run Artifact 提取提出了要求：读取一个巨大的稀疏文件可能展开大量零字节。T10 的分配存储上限不能替代 T14 等后续输出提取路径中的类型、逻辑长度和传输上界检查。

常规 `write` 分配不到空间时可能返回 `ENOSPC`；大块写入也可能先短写，之后才报错。测试应检查实际完成的字节数和随后的错误，不能假设每一次写入都只有“完整成功”和“完全没有写入”两种结果。

`MAP_SHARED` 映射同一个文件时，写入缺失页面仍要经过 tmpfs 分配。额度不足可能使触页进程收到 `SIGBUS`，而不是给 Python 一个可以在普通 `try/except OSError` 中接住的异常。验收应把这种访问放进独立子进程，检查信号结束，并确认整个 Execution 收尾和下一次复用仍正确。`MAP_PRIVATE` 的写时复制页和匿名映射则主要属于内存预算，不能把它们算作写入 Workspace 文件的数据量。

存储拒写与 cgroup OOM 也要分开。tmpfs 文件页计入内存控制；如果 `memory.max` 先成为瓶颈，可能在文件系统用完额度前出现内存分配失败或 OOM。测试 `ENOSPC` 时采用很小的存储预算并保留充足内存；测试 OOM 则查看可信 cgroup 事件。[cgroup 内存字段与边界](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory-interface-files)

Workload 可以捕获 `ENOSPC`、删除不再需要的文件，然后正常返回；内核拒写不要求整次 Execution 一定失败。本次沿用已有退出状态和执行结果，不从异常文本猜测一个新的存储终止原因，也不因最终文件系统恰好已满就把普通退出改成失败。

## 并发写入和跨 Execution 由谁计费

同一 Sandbox 仍只允许顺序 Execution，但一次 Execution 内部可以有线程和后代进程。它们访问同一挂载实例，共享这一个实例的额度。每个子进程不会各自获得一份 512 MiB；存储上界也不依赖 Supervisor 多快轮询一次。

验收并发行为时，不能只运行多个写入者，然后看它们都退出。应统计成功创建的对象或实际分配的数据，确认总量仍有界，并观察某些写入或创建确实在运行中被拒绝。哪个写入者先失败取决于调度，不应成为断言。

Workspace 与 `/tmp` 都在 Sandbox 创建时挂载，随 Sandbox 存活。结束一次 Execution 不会重新挂载或清空它们，所以第一次留下的文件会消耗第二次可用额度。这符合多次 Execution 共享文件状态的执行模型；“临时”表示不作为持久 Attachment 或 Run Artifact 保留，不表示每次 Python 退出就自动清空。

T09 的 Sandbox 父 cgroup 继续存在，使旧 Execution 创建而仍然留存的文件内存与新 Execution 受到共同内存约束。进程迁移或删除一次 Execution 的 cgroup 目录，不等于释放其曾经产生的全部内存。[cgroup Memory Ownership](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory-ownership)

可靠收尾仍然必要：Supervisor 终止本次 Execution 的全部后代，Init 回收并关闭它们持有的资源，再允许下一次执行。只有整个 Sandbox 销毁、相关引用释放后，临时文件系统才结束生命周期。T10 没有引入 Supervisor 崩溃后的持久恢复，也没有负责提取或永久保存文件。

## 为什么不采用其他常见方法

| 方案 | 适合解决什么 | 本项目中的不足或代价 |
| --- | --- | --- |
| 定时 `du` 或遍历目录 | 展示用量、排查异常、做非强制告警 | 检查与写入之间有时间窗口；目录扫描看不到已 unlink 但仍打开的文件；必须明确分配量还是逻辑长度 |
| `inotify` 事件累加 | 观察目录变化、触发索引更新 | 事件是发生后的通知；队列可能溢出，递归目录还要补 watch，不能作为写入前的强制授权 |
| `RLIMIT_FSIZE` | 限制单个进程可以扩展出的文件长度 | 不能限制整个 Sandbox 的聚合文件空间，也不限制大量空文件 |
| cgroup `io.max` | 限制块设备 I/O 的速率和 IOPS | 约束每秒读写量，不是同时占用的存储容量，更不是 tmpfs 文件数 |
| 磁盘文件系统的 project quota | 需要较大容量、磁盘留存或多个项目共享磁盘的目录树预算 | 依赖文件系统支持、挂载与项目 ID 配置，还要设计目录归属、回收和恢复 |
| 自定义 FUSE 文件系统 | 需要特殊计费规则，例如自定义逻辑长度合同 | 增加文件系统守护进程和请求路径，需要处理缓存、并发、映射、取消、阻塞和进程故障 |
| 有限 tmpfs | 短生命周期、体积较小、无需磁盘持久性的 Sandbox 文件状态 | 使用内存资源，需要与 cgroup 共同预算；提供的口径是分配存储与元数据，不是聚合逻辑长度 |

`inotify` 的溢出和新增子目录监听窗口见 [Linux man-pages](https://man7.org/linux/man-pages/man7/inotify.7.html)。`RLIMIT_FSIZE` 的文件长度、`SIGXFSZ` 与 `EFBIG` 语义见 [getrlimit(2)](https://man7.org/linux/man-pages/man2/getrlimit.2.html)。`io.max` 的单位是每秒字节数及操作数，见 [Linux I/O controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#io-interface-files)。

磁盘 project quota 是未来换成磁盘 Workspace 时值得重新评估的方案，而不是“比 tmpfs 更复杂所以永远不要用”。例如 XFS 提供 project quota 的计费和执行选项，见 [Linux XFS 文档](https://docs.kernel.org/admin-guide/xfs.html)。FUSE 则把数据和元数据操作交给用户态文件系统进程；其连接、等待队列和中止机制都是实现需要管理的新增职责，见 [Linux FUSE 文档](https://docs.kernel.org/filesystems/fuse/fuse.html)。表中的适用性判断来自本项目的短生命周期与小容量约束，并非对这些方案的一般性能排名。

## 先运行项目的公开验收

在项目要求的专用 Linux 环境中，准备好 Go、非交互 sudo、锁定的 Profile 源码缓存，以及可委派 `cpu memory pids` 的 cgroup v2 目录。具体准备方式沿用 [T05](t05-sandbox-init.md)、[T09](t09-resource-budgets.md) 和 [Supervisor 协议文档](../sandbox-supervisor-protocol-v1.md)。

```sh
make check

PROFILE_BUNDLE_SOURCE_CACHE=/absolute/path/to/cache \
SANDBOX_TEST_CGROUP_PARENT=/sys/fs/cgroup \
SANDBOX_TEST_RUN='^TestSandboxExecutionStorage' \
bash tests/run-sandbox-init-linux.sh
```

`/sys/fs/cgroup` 只是已具备相应委派条件的环境示例；使用自己为验收准备的目录。脚本构建实际程序和 Profile Bundle，并通过公开 Supervisor 客户端运行 Python；它不是把 tmpfs 挂载选项拼接出来就算通过。

基础构建通过不能证明内核拒写行为，Windows 交叉编译也不能代替真实 Linux 验收。查看测试输出时，应分别确认配置拒绝、空间边界、文件名额、并发累积、跨执行留存和清理后复用，不能把一次正常 Python 输出当成这些条件全部成立。

## 用一个很小的 tmpfs 看清 inode 和稀疏文件

下面是机制实验，独立于本项目的 Sandbox。它需要 Linux、`sudo`、util-linux 和 Python 3；在私有 mount namespace 中创建四页容量、八个 inode 的临时文件系统。它未安装 `python-data-v1` 的 System Call Policy，因而可以观察通常的 `os.link` 行为。它不执行外来代码，也不代表已经验证项目的身份、seccomp 或生命周期合同；不要直接把其中的硬链接操作复制成生产 Profile 的成功断言。

```sh
sudo unshare --mount --propagation private bash <<'SH'
set -euo pipefail
ulimit -c 0
experiment="$(mktemp -d /tmp/t10-tmpfs-XXXXXX)"
trap 'umount "$experiment"; rmdir "$experiment"' EXIT
page="$(getconf PAGESIZE)"
mount -t tmpfs -o "size=$((4 * page)),nr_inodes=8,nosuid,nodev,noexec" tmpfs "$experiment"
python3 - "$experiment" <<'PY'
import errno
import mmap
import os
from pathlib import Path
import signal
import sys

root = Path(sys.argv[1])
page = os.sysconf("SC_PAGE_SIZE")

def require_enospc(operation):
    try:
        operation()
    except OSError as exc:
        assert exc.errno == errno.ENOSPC, exc
    else:
        raise AssertionError("allocation unexpectedly succeeded")

# 根目录占一个名额，另七个由不同类型的目录项共同消耗。
(root / "base").touch()
(root / "directory").mkdir()
(root / "symbolic").symlink_to("base")
for index in range(4):
    os.link(root / "base", root / f"hard-{index}")
require_enospc(lambda: (root / "eighth").touch())
print("directory, symlink and hard links share the inode budget")
for entry in root.iterdir():
    if entry.name == "directory":
        entry.rmdir()
    else:
        entry.unlink()

# 删除名字但保持打开：目录看起来是空的，inode 额度仍被占用。
held = []
for index in range(7):
    path = root / f"held-{index}"
    held.append(os.open(path, os.O_CREAT | os.O_RDWR, 0o600))
    path.unlink()
assert list(root.iterdir()) == []
require_enospc(lambda: (root / "blocked").touch())
for fd in held:
    os.close(fd)
(root / "after-close").touch()
(root / "after-close").unlink()
print("unlinked open files retain their inode slots until released")

# 逻辑长度为八页，只在共享映射触页时分配实际存储。
path = root / "sparse"
fd = os.open(path, os.O_CREAT | os.O_RDWR, 0o600)
os.ftruncate(fd, 8 * page)
print("logical bytes:", os.fstat(fd).st_size)
sys.stdout.flush()
child = os.fork()
if child == 0:
    with mmap.mmap(fd, 8 * page, access=mmap.ACCESS_WRITE) as mapped:
        for offset in range(0, 5 * page, page):
            mapped[offset] = 1
    os._exit(99)
_, status = os.waitpid(child, 0)
assert os.WIFSIGNALED(status), status
assert os.WTERMSIG(status) == signal.SIGBUS, status
require_enospc(lambda: os.pwrite(fd, b"x", 6 * page))
os.close(fd)
path.unlink()
print("shared mmap hit SIGBUS; another file write hit ENOSPC")

(root / "reused").write_bytes(b"usable after release")
print((root / "reused").read_text())
PY
SH
```

预期观察是：目录与链接共同耗尽 inode；没有路径的打开文件仍占额度；逻辑长度可以超过四页，但第五个实际数据页不能成功分配；释放资源后可以再次写入。这里的“四页”和“八个 inode”是实验配置，不是项目测试测得的数据。上面的预期也不替代运行记录。

实验在子进程中触发 `SIGBUS`，防止整个观察进程在拿到结果前退出；`ulimit -c 0` 避免生成 core dump。实际 Workload 的信号回收由 Sandbox Init 和现有 Execution 生命周期处理，需要用项目验收另外证明。

## 代码阅读路径

| 顺序 | 文件 | 要回答的问题 |
| --- | --- | --- |
| 1 | [StorageBudget](../../internal/sandboxsupervisor/storage_budget_linux.go) | 哪些值可配置？为什么零值和非页对齐必须拒绝？文件数如何避免溢出？ |
| 2 | [ResourceBudget](../../internal/sandboxsupervisor/resource_budget_linux.go) | 存储如何成为整体预算的一部分？默认值怎样进入服务？ |
| 3 | [sandboxd CLI](../../cmd/sandboxd/main_linux.go) | 哪些参数属于 Operator？私有 bootstrap 入口与公开服务入口怎样区分？ |
| 4 | [Sandbox 创建](../../internal/sandboxsupervisor/creation_linux.go) | 已验证的配置如何成为私有启动参数？失败时资源仍由谁管理？ |
| 5 | [bootstrap](../../internal/sandboxsupervisor/bootstrap_linux.go) | 为什么先挂载再启动 Init？三个 Workspace 可信 inode 在哪里产生？ |
| 6 | [System Call Policy](../../profiles/python-data-v1/system-call-policy.json) | Workload 为什么不能直接 remount 放宽预算？允许哪些文件操作？ |
| 7 | [Execution](../../internal/sandboxsupervisor/execution_linux.go) 与 [Init](../../internal/sandboxsupervisor/init_linux.go) | 文件还被后代持有时，何时才允许下一次 Execution？ |
| 8 | [存储验收](../../tests/sandbox_storage_linux_test.go) | 是否真正写满、建满并观察拒绝？是否通过同一 Sandbox 验证第二次执行？ |

源码阅读时可以同时打开固定版本的 [Linux v6.8 `mm/shmem.c`](https://github.com/torvalds/linux/blob/v6.8/mm/shmem.c)：查找 `shmem_reserve_inode`、`shmem_link`、`shmem_unlink`、`shmem_evict_inode`、`shmem_inode_acct_blocks` 与 `shmem_fill_super`。本文使用该版本解释计费逻辑；实际验收需要另记运行内核版本，不能把引用内核版本当成本项目测试环境。

## 面试时可以怎样解释

**为什么有了 cgroup 内存上限，还要限制 tmpfs？**

内存预算约束整个 Sandbox，存储预算约束其中的文件空间。单靠内存上限，写文件可能挤压 Python 和 Init，最终以 OOM 收尾；独立文件系统容量能更早在文件操作边界拒绝分配。文件数量也需要单独限制，因为空文件仍消耗元数据。

**怎么证明限制对并发和子进程有效？**

预算属于挂载实例，不属于某个 Python PID。多个写入者访问同一实例，内核在分配时检查共同额度；测试通过并发实际分配和总量边界证明，不能只展示挂载参数。

**为什么不给每次 Execution 重新发一份空 Workspace？**

本项目的一次 Agent Run 可能包含多个顺序 Execution，需要复用前一步生成的文件。挂载和父预算随 Sandbox 存活，已有文件继续计费；每次只更换并回收 Workload 进程。这个选择由 Agent Run 的状态延续需求决定。

**5000 个文件怎么避免硬链接绕过？**

先区分策略是否允许操作，再解释文件系统怎样计费。当前 Python `os.link` 路径返回 `EPERM`，但策略已经允许传统 `link` 系统调用。Profile 验收通过 `ctypes` 调用 Linux amd64 的 `SYS_link=86`，验证 tmpfs 对额外硬链接的计费，不为测试放宽策略。目录、符号链接、已删除但仍打开的文件也纳入真实内核验收，结果见下面的验证记录。

**为什么超额后不总是返回统一的 storage_limit？**

普通写入可能短写或 `ENOSPC`，共享映射可能 `SIGBUS`，内存先不足时还可能 OOM。程序也可能捕获拒写并恢复。平台保留已经能够可信观察的结果，不从用户输出或最终目录状态猜一个无法严格归因的原因。

**一个 10 GiB 的稀疏文件出现了，是不是预算失效？**

要先区分逻辑长度与分配容量。T10 限制分配的存储；如果只扩展了洞，不能据此认定越界。继续验证实际分配是否受限，同时指出下游产物导出必须有独立的逻辑长度与传输限制。

**什么时候会改用磁盘 project quota？**

当 Workspace 容量、持久性或内存成本使 tmpfs 不再合适时重新评估。届时要把文件系统支持、项目身份分配、目录继承、失败回收与恢复一起设计，仍然保留内核强制和真实边界验收，而不只是把挂载类型换掉。

## 验证记录

2026-09-08，在基准提交 `9a77dfa8a53d82df94902b6299483cf007c2bf9c` 加本次 T10 变更的源码上验证。使用 Go 1.26.1 和一次性 QEMU 虚拟机中的 Linux `6.12.107-0-virt`，启用真实 cgroup v2、namespace、tmpfs 和锁定 CPython Profile Bundle。

开发过程先观察公开验收失败，再补实现：最初 `--workspace-bytes`、文件名额参数和 `--temporary-bytes` 均未被识别；输入保护验收还发现 `/workspace/input` 没有预留。完成配置传递和目录创建后，同一验收通过。硬链接实验最初收到 `EPERM`，经核对 CPython 与 Profile 策略后改为使用已经允许的 `SYS_link`，没有扩大白名单。

| 验证范围 | 需要保留的证据 | 当前记录 |
| --- | --- | --- |
| 配置与公开协议 | 可信参数、零值/负值/页对齐/溢出拒绝、请求不能覆盖预算 | 两项公开协议测试及其无效配置子用例通过 |
| 数据容量 | 实际写入至边界、拒写、空间回收后可复用 | 默认 512 MiB 写满后拒写；8 KiB 自定义预算跨 Execution 留存、拒写、释放与复用通过 |
| 文件名额与策略 | 普通文件、目录、符号链接、允许的 `SYS_link` 硬链接、打开后 unlink 的计费 | 默认 5,000 名额及 4 名额混合元数据验收通过；最后引用关闭后额度恢复 |
| `/tmp` | 独立容量和名额，不能通过切换目录消耗另一处额度 | 默认 16 MiB / 1,023 名额、自定义 4 KiB / 2 名额及独立占用通过 |
| 并发与跨执行 | 多写入者共用额度；同一 Sandbox 的后续 Execution 仍受已用容量约束 | 4 个子进程共同受 32 KiB / 4 名额约束；跨执行与不同 Sandbox 隔离、销毁后清空通过 |
| 稀疏文件与映射 | 逻辑长度和分配量区分；共享映射拒绝及信号后的清理 | 1 GiB 逻辑文件受 4 KiB 分配预算限制；共享映射超额触发 `SIGBUS`，后续 Execution 可复用 |
| 保护边界 | 超额后 Profile、预留输入及宿主文件保持不变 | 实际耗尽容量和名额后，写入/替换输入目录、修改 Profile 和宿主哨兵文件均被拒绝 |
| 执行与生命周期回归 | 公开脚本默认选择的 Execution 与 Lifecycle 真实内核套件 | 47 个顶层测试全部通过、无跳过，包含新增 9 个 T10 存储验收 |
| 创建回归 | `bash tests/run-sandbox-creation-linux.sh` | 10 个真实内核顶层测试全部通过、无跳过，覆盖创建、根目录、身份映射及描述符 |
| 仓库通用检查 | 普通 Linux UID 1000，逐项运行 Makefile `check` 的全部命令 | Go 格式、`go vet ./...`、`go test -v ./...`、`go build ./...` 及四项 Shell 语法检查全部通过 |
| 独立代码审查 | 相对基准提交的完整变更，规范和规格两项分别检查 | Standards 0 项发现；Spec 0 项发现 |

测试虚拟机没有 GNU make，因此通用检查执行了 Makefile 中的完整等价命令，没有将它记作实际运行 `make check`。最初虚拟机的回环网卡未启用，已有数据库连接超时测试得到连接失败而非超时；启用虚拟机回环接口后通过，未修改项目代码。没有配置 `AGENT_TEST_DATABASE_URL`，6 个顶层数据库测试和历史校验下的 3 个子用例明确跳过；本次未运行 PostgreSQL 专用验收、竞态构建或性能压测。

上面的独立 tmpfs 教学脚本进行了静态语法检查，未作为本次通过的测试结果计数。可以依照脚本自行复现其预期输出。

这些证据只支持 T10 及相关回归范围，不等于已经完成真实 Attachment 绑定、Run Artifact 提取、跨重启恢复、所有生产隔离合同或性能压测。
