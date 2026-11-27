# G6 性能对照方法（3.5.25）

## 环境
- 两版同镜像 `golang:1.27.3`（Go 1.27.3 linux/amd64）、同宿主硬件（12th Gen i3-12300T）、同工作负载。
- 3.5.24 代码树：`ml3525-g0-base` 容器（基线 archive，commit=84b013d…）。
- 3.5.25 代码树：`ml3525-int-build` 容器（commit=0caf9da…）。
- 差异仅在源码，Go 编译器、GOARCH、GC 默认参数一致。

## 采集
- 现成 store benchmark：`go test ./internal/store/ -run '^$' -bench '…' -benchtime=100x -count=5 -benchmem`。
- 针对性基准：`BenchmarkAddSourceHostG6`（同 DB 循环，排除建库开销）注入两版容器各自运行。
- 每场景 5 轮独立测量，每轮 100 次操作；P50/P95 基于全部样本（非对 5 轮结果再算 P95）。

## 门槛
- §17.3 新回归门槛：P50 增幅 ≤10%、P95 增幅 ≤15%。
- 缓存性能门槛（热 P95≤2ms、冷比值≤1.5、并发成功率100%）仅在 gateway 热/冷缓存路径适用，本次隔离基准未覆盖该路径，标 NOT_RUN。

## 边界（诚实声明）
- 本次为本地隔离基准，未覆盖 §17.1 要求的真实源站/网络/TLS/UI，不构成完整 G6 PASS。
- 真实源站对照受 G5 同一限制（无 SparkCR/SUNBALCONY 端点、无 NAS 权限），标 BLOCKED_REAL_ENV。
- 不通过关闭 TLS/缩短安全检查换取速度。
