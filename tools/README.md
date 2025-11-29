# GridKV Benchmark Tools

快速基准测试工具，用于在每个优化步骤后验证性能提升。

## 快速开始

### 1. 设置基线（首次运行）

```bash
# 运行基准测试并自动设置为基线
./tools/run_benchmark.sh baseline
```

### 2. 运行优化后的基准测试

```bash
# 使用默认配置（5节点，30秒，100并发）
./tools/run_benchmark.sh phase1_read_separation

# 自定义配置
GRIDKV_BENCH_NODES=10 \
GRIDKV_BENCH_DURATION=60s \
GRIDKV_BENCH_CONCURRENT=200 \
./tools/run_benchmark.sh phase2_batch_optimization
```

### 3. 查看结果

结果会自动与基线对比，显示关键指标的改进/回退情况。

## 配置选项

环境变量：

- `GRIDKV_BENCH_NODES`: 节点数量（默认: 5）
- `GRIDKV_BENCH_DURATION`: 测试时长（默认: 30s）
- `GRIDKV_BENCH_CONCURRENT`: 并发操作数（默认: 100）
- `GRIDKV_BENCH_NETWORK`: 网络类型，TCP/QUIC/UDP（默认: TCP）
- `GRIDKV_BENCH_BACKEND`: 存储后端，Memory/MemorySharded（默认: MemorySharded）

## 输出指标

基准测试测量以下关键指标：

### 性能指标
- **Total QPS**: 总操作数/秒
- **Write QPS**: 写操作数/秒
- **Read QPS**: 读操作数/秒
- **Delete QPS**: 删除操作数/秒

### 成功率
- **Write Success Rate**: 写操作成功率
- **Read Success Rate**: 读操作成功率
- **Delete Success Rate**: 删除操作成功率

### 延迟
- **P50/P95/P99 Latency**: 总体延迟百分位
- **Read Latency P50/P95/P99**: 读操作延迟百分位

### 资源占用
- **Peak/Final Goroutines**: 峰值/最终 goroutine 数量
- **Peak/Final Memory**: 峰值/最终内存占用（MB）

## 结果文件

所有结果保存在 `benchmark_results/` 目录：
- JSON 格式的详细结果
- 日志文件（`.log`）
- 基线文件（`baseline.json`）

## 使用场景

### 优化步骤验证

在每个优化阶段完成后运行：

```bash
# Phase 1: 读写分离
./tools/run_benchmark.sh phase1_read_separation

# Phase 2: 读路径优化
./tools/run_benchmark.sh phase2_read_optimization

# Phase 3: Gossip 优化
./tools/run_benchmark.sh phase3_gossip_optimization
```

### CI/CD 集成

```bash
# 在 CI 中运行
./tools/run_benchmark.sh ci_run_$(date +%s)

# 检查是否通过阈值（需要实现check_thresholds.go）
# if ! go run tools/check_thresholds.go benchmark_results/latest.json; then
#     echo "Performance regression detected!"
#     exit 1
# fi
```

## 性能阈值

建议的验收标准（参考 `OPTIMIZATION_PLAN.md`）：

- **读 QPS**: ≥ 350（目标）
- **读成功率**: ≥ 65%
- **写 QPS**: 保持基线水平或提升
- **P99 延迟**: 相比基线不上升 > 20%
- **Goroutine 数量**: 相比基线不增加 > 50%

## 手动对比

```bash
go run tools/compare/main.go \
    benchmark_results/baseline.json \
    benchmark_results/phase1_read_separation_20241129_120000.json
```

## 注意事项

1. **环境一致性**: 确保测试环境稳定，避免其他进程干扰
2. **资源充足**: 确保有足够内存和 CPU 资源
3. **多次运行**: 建议运行多次取平均值，减少波动
4. **基线更新**: 每次重大优化后更新基线

