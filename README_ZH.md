# GridKV

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[English](README.md) | 简体中文

**GridKV** 是一个 Go 原生嵌入式分布式键值缓存，具有自动集群、批量最终一致性复制和自适应网络功能。

---

## 特性

- **嵌入式**: 作为 Go 库导入，进程内运行
- **分布式 Get**: 自动副本选择、重试和读修复
- **自动集群**: SWIM 协议，<1 秒故障检测
- **高性能**: 单机峰值可达 3 节点 94K ops/s（Memory） / 66K ops/s（MemorySharded），本地微基准最高 14.6M ops/s
- **自适应网络**: 自动检测 LAN/WAN，相应优化

---

## 快速开始

```bash
go get github.com/feellmoose/gridkv
```

```go
package main

import (
    "context"
    "log"
    gridkv "github.com/feellmoose/gridkv"
)

func main() {
    kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
        LocalNodeID:  "node-1",
        LocalAddress: "localhost:8001",
        SeedAddrs:    []string{"localhost:8002", "localhost:8003"},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer kv.Close()
    
    ctx := context.Background()
    kv.Set(ctx, "user:1001", []byte("Alice"))
    value, _ := kv.Get(ctx, "user:1001")
    log.Printf("Value: %s", value)
}
```

---

## 分布式 Get

GridKV 的 `Get()` 操作提供智能分布式读取能力：

### 能力

- **自动副本选择**: 使用一致性哈希定位 N 个副本
- **本地优先**: 如果本地有数据立即返回（43ns）
- **智能转发**: 非本地数据自动转发到协调节点
- **快速重试**: 失败时自动重试，指数退避
- **读修复**: 检测版本不一致并修复过期副本
- **热缓存**: 频繁访问的键本地缓存（<1ms 延迟）

### 性能

所有数据均来自单台 Linux 6.14 主机（Go 1.23、Intel i7-12700H、20 线程、GOMAXPROCS=20）并使用 QUIC 传输。我们同时公布两种模式：

- **峰值模式**：执行 `GRIDKV_NO_THROTTLE=1 go test ./tests/... -run TestMemory*` 关闭 `runMixedClusterWorkload` 内的 10 ms / 5 ms / 20 ms 休眠，以获得单机的最大吞吐。
- **验证模式**：默认节流（60% 写 / 30% 读 / 10% 删），持续用于回归测试内存、安全与压缩路径。

### 峰值集群吞吐（QUIC，`GRIDKV_NO_THROTTLE=1`）

| MemorySharded 集群 | 总 QPS | 写 QPS | 读 QPS | 删 QPS |
|--------------------|--------|--------|--------|--------|
| 3 节点 | 66,184 | 63,866 | 688 | 1,630 |
| 5 节点 | 17,154 | 15,395 | 1,264 | 495 |
| 10 节点 | 10,059 | 9,390 | 216 | 454 |
| 15 节点 | 8,979 | 8,542 | 172 | 266 |

| Memory 集群 | 总 QPS | 写 QPS | 读 QPS | 删 QPS |
|--------------|--------|--------|--------|--------|
| 3 节点 | 94,625 | 61,709 | 30,564 | 2,352 |
| 5 节点 | 38,396 | 37,300 | 241 | 856 |
| 10 节点 | 9,039 | 8,452 | 120 | 467 |

_说明：由于本机 UDP 接收缓冲仅 416 KiB，小集群的 QUIC 读吞吐会提前受限。提高 `rmem_max` 或多机部署后，读性能会显著提升。_

### 验证集群吞吐（混合 60/30/10，默认节流）

| MemorySharded 集群 | 总 QPS | 写 QPS | 读 QPS | 删 QPS |
|--------------------|--------|--------|--------|--------|
| 3 节点 | 5,627 | 1,921 | 3,246 | 459 |
| 5 节点 | 3,430 | 1,959 | 794 | 677 |
| 10 节点 | 1,476 | 758 | 268 | 450 |
| 15 节点 | 1,990 | 928 | 728 | 334 |

| Memory 集群 | 总 QPS | 写 QPS | 读 QPS | 删 QPS |
|--------------|--------|--------|--------|--------|
| 3 节点 | 4,452 | 1,634 | 2,434 | 383 |
| 5 节点 | 2,725 | 1,630 | 529 | 566 |
| 10 节点 | 1,621 | 797 | 211 | 613 |

### 本地微基准（`go test ./internal/storage -run ^$ -bench BenchmarkMemory -benchtime=2s`）

| 后端 | 场景 | Ops/sec | 备注 |
|------|------|---------|------|
| Memory | 小值 100B | 3.35M | 306 ns/op |
| Memory | 中值 4KB（可压缩） | 0.77M | 1.32 µs/op，压缩后 0.8 % |
| Memory | 大值 16KB（可压缩） | 0.32M | 3.23 µs/op，压缩后 0.2 % |
| Memory | 写密集 100B | 3.30M | 308 ns/op |
| Memory | 读密集热数据 | 8.91M | 115 ns/op |
| MemorySharded | 小值 100B | 6.28M | 159 ns/op |
| MemorySharded | 写密集 100B | 6.63M | 151 ns/op |
| MemorySharded | 读密集热数据 | 14.6M | 68.6 ns/op |

**延迟（MemorySharded / QUIC）**:
- 本地（同一实例）: 43 ns
- 分布式 Get (LAN, 缓存): <1 ms
- 分布式 Get (LAN, 转发): ~2 ms
- 分布式 Get (WAN, 缓存): <50 ms
- 分布式 Set (异步): ~2 ms

---

## 存储后端

| 后端 | 峰值集群 (10 节点，QUIC，**无节流**) | 验证集群 (10 节点，QUIC，节流) | 本地微基准 (ops/s) | 使用场景 | 说明 |
|------|------------------------------------|------------------------------------|----------------------|----------|------|
| **MemorySharded** | **10.1K ops/s**（写 9.4K / 读 0.2K / 删 0.5K） | **1.48K ops/s**（写 0.76K / 读 0.27K / 删 0.45K） | 6.28M (100B 小值)<br>14.6M (读密集) | 生产环境，高吞吐量 | 峰值模式通过 `GRIDKV_NO_THROTTLE=1` 打开。验证模式保留 10 ms / 5 ms / 20 ms 的写/读/删休眠。 |
| **Memory** | **9.0K ops/s**（写 8.5K / 读 0.1K / 删 0.5K） | **1.62K ops/s**（写 0.80K / 读 0.21K / 删 0.61K） | 3.35M (100B 小值)<br>8.91M (读密集) | 开发、测试 | 压缩友好的 4KB / 16KB 值可达 0.77M / 0.32M ops/s。设置 `GRIDKV_NO_THROTTLE=1` 可获得峰值集群数据。 |

### MemorySharded（推荐）

**峰值集群吞吐**（QUIC，`GRIDKV_NO_THROTTLE=1`，单台 i7-12700H）：
- **3 节点**：66.2K ops/s（写 63.9K / 读 0.7K / 删 1.6K）
- **5 节点**：17.2K ops/s（写 15.4K / 读 1.3K / 删 0.5K）
- **10 节点**：10.1K ops/s（写 9.4K / 读 0.2K / 删 0.5K）
- **15 节点**：9.0K ops/s（写 8.5K / 读 0.2K / 删 0.3K）

**验证集群吞吐**（默认 10 ms / 5 ms / 20 ms 节流）：
- **3 节点**：5.63K ops/s（写 1.92K / 读 3.25K / 删 0.46K）
- **5 节点**：3.43K ops/s（写 1.96K / 读 0.79K / 删 0.68K）
- **10 节点**：1.48K ops/s（写 0.76K / 读 0.27K / 删 0.45K）
- **15 节点**：1.99K ops/s（写 0.93K / 读 0.73K / 删 0.33K）

_传输说明：当前主机的 UDP 接收缓冲仅 416 KiB，QUIC 小集群的读吞吐会提前受限。调高 `rmem_max` 或多机部署可显著提升读性能。_

**本地微基准**（`go test ./internal/storage -run ^$ -bench BenchmarkMemorySharded -benchtime=2s`）：
- 小值 (100B)：6.28M ops/s (159 ns/op)
- 写密集 (100B)：6.63M ops/s (151 ns/op)
- 读密集：14.6M ops/s (68.6 ns/op，100% 命中窗口)

**配置**:
```go
Storage: &gridkv.StorageOptions{
    Backend:     gridkv.BackendMemorySharded,
    ShardCount:  256,     // CPU 核心数的 2-4 倍
    MaxMemoryMB: 4096,
}
```

### Memory（简单）

**峰值集群吞吐**（QUIC，`GRIDKV_NO_THROTTLE=1`，单台 i7-12700H）：
- **3 节点**：94.6K ops/s（写 61.7K / 读 30.6K / 删 2.4K）
- **5 节点**：38.4K ops/s（写 37.3K / 读 0.24K / 删 0.86K）
- **10 节点**：9.0K ops/s（写 8.45K / 读 0.12K / 删 0.47K）

**验证集群吞吐**（默认节流）：
- **3 节点**：4.45K ops/s（写 1.63K / 读 2.43K / 删 0.38K）
- **5 节点**：2.73K ops/s（写 1.63K / 读 0.53K / 删 0.57K）
- **10 节点**：1.62K ops/s（写 0.80K / 读 0.21K / 删 0.61K）

_传输说明：去除节流后，瓶颈从应用逻辑转移到传输层；在本机上 TCP 峰值与 QUIC 峰值在 ±30% 范围内波动。_

**本地微基准**（`go test ./internal/storage -run ^$ -bench BenchmarkMemory -benchtime=2s`）：
- 小值 (100B)：3.35M ops/s (306 ns/op)
- 中值 (4KB，可压缩)：0.77M ops/s (1.32 µs/op，压缩后 0.8 %)
- 大值 (16KB，可压缩)：0.32M ops/s (3.23 µs/op，压缩后 0.2 %)
- 写密集 (100B)：3.30M ops/s (308 ns/op)
- 读密集：8.91M ops/s (115 ns/op)

_测试环境：Linux 6.14、Go 1.23、Intel i7-12700H（20 逻辑核）、GOMAXPROCS=20。_

**配置**:
```go
Storage: &gridkv.StorageOptions{
    Backend:     gridkv.BackendMemory,
    MaxMemoryMB: 2048,
}
```

---

## 传输层

| 传输 | 延迟 (LAN) | 延迟 (WAN) | 可靠性 | 使用场景 |
|------|------------|------------|--------|----------|
| **TCP** | ~1-5ms | ~20-100ms | ✅ 保证 | 生产环境，最大兼容性 |
| **QUIC** | ~0.5-2ms | ~10-50ms | ✅ 保证 | 大集群，高性能 |
| **UDP** | ~0.1-1ms | ~5-20ms | ❌ 尽力而为 | 超低延迟（谨慎使用） |

### TCP（兼容性推荐）

- 可通过所有防火墙/NAT
- 保证交付和顺序
- 适合生产环境部署

```go
Network: &gridkv.NetworkOptions{
    Type:     gridkv.TCP,
    BindAddr: "0.0.0.0:8001",
    MaxConns: 256,
}
```

### QUIC（性能推荐）

- 0-RTT 连接建立
- 单连接多路复用
- CPU 使用率较高（加密开销）

```go
Network: &gridkv.NetworkOptions{
    Type:     gridkv.QUIC,
    BindAddr: "0.0.0.0:8001",
    MaxConns: 512,
}
```

### UDP（谨慎使用）

- 最低延迟，无连接开销
- **不保证交付** - 消息可能丢失
- **不推荐用于生产环境**

---

## 性能

> **测试场景**：除非特别说明，以下数据均来自单台 Linux 6.14 主机（Go 1.23、Intel i7-12700H、20 线程、GOMAXPROCS=20），使用 LAN 配置、QUIC 传输以及共享的 `runMixedClusterWorkload` 脚本（约 60% 写 / 30% 读 / 10% 删；写/读/删线程分别休眠 10ms/5ms/20ms）。并发量随集群规模增加（Memory：60/100/180 worker；MemorySharded：80/140/220/320 worker）。
>
> **重要提示**：当前所有性能数据都来自单机 LAN 模拟环境，我们只验证到 15 节点集群。多机部署或更大规模的集群在真实网络下的表现仍需额外压测。

**集群吞吐量**（MemorySharded，QUIC，LAN，单台 i7-12700H）：

| 集群大小 | 总 QPS | 写 QPS | 读 QPS | 删 QPS |
|----------|--------|--------|--------|--------|
| 3 节点 | 3,286 | 2,664 | 14 | 608 |
| 5 节点 | 5,548 | 4,477 | 13 | 1,058 |
| 10 节点 | 8,533 | 6,897 | 11 | 1,625 |
| 15 节点 | 1,351 | 1,087 | 11 | 253 |

**Memory 后端吞吐量**（同一场景）：

| 集群大小 | 总 QPS | 写 QPS | 读 QPS | 删 QPS |
|----------|--------|--------|--------|--------|
| 3 节点 | 2,462 | 1,986 | 11 | 465 |
| 5 节点 | 3,928 | 3,179 | 9 | 740 |
| 10 节点 | 6,903 | 5,540 | 8 | 1,355 |

> 同压测脚本下的 TCP 基线（具体比例见各后端章节）大约是 QUIC 的 30%~190%，差异来自单机 UDP 缓冲限制与 CPU 竞争。多机部署通常能获得更高吞吐；本场景主要用于验证功能、内存与协程安全。

**延迟（MemorySharded / QUIC）**:
- 本地（同一实例）: 43ns
- 分布式 Get (LAN, 缓存): <1ms
- 分布式 Get (LAN, 转发): ~2ms
- 分布式 Get (WAN, 缓存): <50ms
- 分布式 Set (异步): ~2ms

---

## 配置

### 最小配置

```go
kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "node-1",
    LocalAddress: "localhost:8001",
    SeedAddrs:    []string{"localhost:8002"},
})
```

### 生产配置

```go
kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "node-1",
    LocalAddress: "10.0.1.10:8001",
    SeedAddrs:    []string{"10.0.1.11:8001", "10.0.1.12:8001"},
    
    Network: &gridkv.NetworkOptions{
        Type:     gridkv.QUIC,  // 或 gridkv.TCP
        BindAddr: "0.0.0.0:8001",
        MaxConns: 512,
    },
    
    Storage: &gridkv.StorageOptions{
        Backend:     gridkv.BackendMemorySharded,
        ShardCount:  256,
        MaxMemoryMB: 4096,
    },
    
    ReplicaCount: 3,
    TTL:          24 * time.Hour,
})
```

---

## API

```go
// Set: 存储并自动复制
func (g *GridKV) Set(ctx context.Context, key string, value []byte, ttl ...time.Duration) error

// Get: 分布式读取，自动重试和读修复
func (g *GridKV) Get(ctx context.Context, key string) ([]byte, error)

// GetAsync: 异步读取，返回 Future
func (g *GridKV) GetAsync(ctx context.Context, key string) (ReadFuture, error)

// GetBatchAsync: 批量异步读取
func (g *GridKV) GetBatchAsync(ctx context.Context, keys []string) (BatchReadFuture, error)

// Delete: 删除键（墓碑异步复制）
func (g *GridKV) Delete(ctx context.Context, key string) error

// Close: 优雅关闭
func (g *GridKV) Close() error
```

**线程安全**: 所有方法都支持并发访问。

---

## 许可证

MIT License - 参见 [LICENSE](LICENSE)

---

<div align="center">

**GridKV** - Go 原生嵌入式分布式缓存

*高性能 • 自动集群 • 零外部依赖*

</div>
