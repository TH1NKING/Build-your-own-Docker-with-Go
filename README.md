最初版本:用shell+busybox实现了环境隔离，具体为：run-mini-container.sh设置严格模式 & 设定 rootfs 路径、检查 rootfs 是否存在、在宿主机上创建 rootfs 的挂载点目录、在宿主机上把宿主的 /dev bind 到 rootfs 的 /dev、
在宿主机上把 sysfs 挂到 rootfs 的 /sys、如果 rootfs 的 /proc 已经被挂载过，就先在宿主机卸载、创建一组新的 namespace，然后 chroot 进去跑一个 shell；cleanup-minicontainer.sh定义一个通用卸载函数并按顺序尝试卸载 
rootfs 里的 proc/sys/dev。

shell1.1更新（written by ChatGPT）：
1) 不再污染宿主机：退出后基本不需要 cleanup
你原来的 run-mini-container.sh 在 unshare --mount ... 之前就对 $ROOTFS/dev、$ROOTFS/sys 做了 mount（bind/sysfs）——这发生在宿主机 mount namespace，所以退出容器后挂载还在，只能靠 cleanup-minicontainer.sh 手动卸载。
新脚本：先 unshare --mount，再 mount。
结果是：
容器退出 = mount namespace 结束 = 这些挂载自动消失
宿主机不会留下 $ROOTFS/dev|sys|proc 的挂载残留
这就是最直接的“少一堆清理脚本 & 少一堆事故”。
2) 隔离更“真”：挂载变化不会传播回宿主机
你现在的方式本质上是“宿主机帮容器准备好 rootfs 的挂载环境”。这不算严格意义的容器挂载隔离。
新脚本把 mount 全放到容器的 mount namespace，容器里怎么 mount/unmount，只影响容器自己。
3) /proc 正常可用（PID namespace 下非常关键）
你原脚本把 mount -t proc proc /proc 注释了，但你用了 --pid --fork。没有 /proc 很多常用命令会异常。
新脚本会把 proc 挂进 $ROOTFS/proc，chroot 后就是 /proc，容器里的进程视图更正常。
4) 防止“挂载传播”的坑：mount --make-rprivate /
这是你原脚本没有做的。某些发行版/场景下，mount 传播（shared/private）会让你出现很诡异的“我明明在容器里 mount，宿主机也跟着变”的现象。
新脚本在新 namespace 里把 / 设为 rprivate，避免意外传播，稳定性更好。
5) 权限处理更干净：一次 sudo，避免到处 sudo
你原脚本在多个 mount 点位上单独 sudo。新脚本开头统一检测 EUID，不够就 exec sudo -E "$0" "$@"，后面就都不需要反复 sudo 了（更易读也更少“半失败状态”）。

shell1.2更新：lo is up。
