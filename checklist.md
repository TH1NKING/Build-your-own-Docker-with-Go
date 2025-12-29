1) 进程与 /proc（PID namespace 是否正常）

容器里运行：

echo "PID=$$"
ps -ef | head
cat /proc/self/status | egrep 'Name|Pid|PPid|NSpid' -n
ls /proc | head


你想看到的：

ps 能用

/proc/self/status 里有 NSpid（能反映 PID ns）

/proc 不是空目录

如果这些不正常，通常就是 /proc 没挂好。

2) 挂载与 rootfs（mount namespace 是否干净）

容器里：

mount | egrep ' /$| /proc | /sys | /dev ' 
df -hT | head


宿主机（退出容器后再检查一次，非常关键）：

ROOTFS=$HOME/mydocker/rootfs
mount | grep "$ROOTFS" || true


你想看到的：

容器内有 /proc(proc)、/sys(sysfs)、/dev(bind 或你后续做的 devpts)

宿主机退出后 没有 $ROOTFS/dev|sys|proc 残留挂载

3) UTS（hostname 隔离）

容器里：

hostname
cat /etc/hostname 2>/dev/null || true


宿主机对照：

hostname


你想看到的：容器 hostname 改了也不影响宿主机。

4) IPC namespace（最简单的验证）

容器里：

ipcs -a | head


宿主机再看一眼（不用对比输出内容，主要是后面你做共享时才更明显）。

5) Network namespace（从“有 lo”到“能出网”）

容器里先检查：

ip a
ip route
ip link show lo


最小可用（至少回环通）：

ip link set lo up
ping -c 1 127.0.0.1


下一阶段（你做 veth + NAT/bridge 后）再验收：

ping -c 1 <宿主机侧 veth/网关IP>
ping -c 1 1.1.1.1
nslookup openai.com 2>/dev/null || true


你想看到的：

现在阶段：至少 lo up 后 127.0.0.1 通

做完网络：能 ping 外网 IP、能解析域名（DNS）

6) 文件系统可写性 & 基础工具（rootfs 是否“能生活”）

容器里：

pwd
ls -la /
touch /tmp/mini_test && echo ok > /tmp/mini_test && cat /tmp/mini_test
id
whoami
uname -a


你想看到的：

/tmp 可写

基础命令存在且工作正常

7) /dev 是否“够用”（尤其是终端/伪终端）

容器里：

ls -l /dev | head -n 30
test -c /dev/null && echo "/dev/null ok"
echo "hello" > /dev/null && echo "write null ok"


如果你后面做 devpts，再测：

tty
ls -l /dev/pts


你想看到的：

/dev/null 等基本节点可用

有交互 shell 时，tty 正常、/dev/pts 工作

8) 退出清理（“像容器”的关键体验）

退出容器后宿主机立刻跑：

ROOTFS=$HOME/mydocker/rootfs
mount | grep "$ROOTFS" || echo "no leftover mounts"


你想看到的：没有残留（如果有，说明 mount 还在宿主机做了或传播没处理好）。

9)（后面做 cgroup 时）资源限制验收

等你上 cgroup v2 后，再加这几条：

容器里：

cat /proc/self/cgroup


压测/触发限制（例子）：

pids：疯狂 fork（谨慎，先小限制）

memory：分配内存直到失败，看是否 OOM/被限制
