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
- **高性能**: 3-6M ops/s（5节点集群），10-20M ops/s（10节点集群）
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

**集群吞吐量** (MemorySharded 后端, QUIC 传输):
- **5节点集群**: 3-6M ops/s (LAN)
- **10节点集群**: 10-20M ops/s (LAN)
- **15节点集群**: 15-30M ops/s (LAN)

**延迟**:
- 本地（同一实例）: 43ns
- LAN（缓存，单副本）: <1ms
- LAN（转发）: ~2ms
- WAN（缓存）: <50ms

### 工作原理

1. **哈希键** → 通过一致性哈希环找到 N 个副本节点
2. **检查本地** → 如果本地节点是副本，立即返回
3. **转发请求** → 如果非本地，转发到协调节点（第一个副本）
4. **失败重试** → 自动重试，快速退避
5. **读修复** → 如果多个副本返回不同版本，使用最新版本并修复过期副本

### 示例

```go
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
defer cancel()

// Get 自动处理：
// - 找到正确的副本
// - 失败时重试
// - 修复不一致
value, err := kv.Get(ctx, "user:12345")
if err != nil {
    log.Printf("Get 失败: %v", err)
    return
}
// 值保证是最新的可用版本
```

---

## 存储后端

| 后端 | 集群性能 (10节点) | 本地性能 | 使用场景 | 说明 |
|------|------------------|----------|----------|------|
| **MemorySharded** | 10-20M ops/s (混合)<br>30-50M ops/s (读密集型) | 5.8M ops/s (100B)<br>14M ops/s (读密集型) | 生产环境，高吞吐量 | 生产推荐。分片数：CPU 核心数的 2-4 倍。 |
| **Memory** | 2-4M ops/s (混合)<br>5-8M ops/s (读密集型) | 933K ops/s (100B)<br>2.2M ops/s (读密集型) | 开发，测试 | 对 >1KB 的值自动压缩。单锁限制并发。 |

### MemorySharded（推荐）

**集群性能**:
- **5节点集群**: 3-6M ops/s (混合工作负载)
- **10节点集群**: 10-20M ops/s (混合工作负载)
- **15节点集群**: 15-30M ops/s (混合工作负载)
- **读密集型 (10节点)**: 30-50M ops/s

**本地性能** (单节点, Intel i7-12700H, 20 核):
- 小值 (100B): 5.8M ops/s (170ns/op)
- 中值 (1KB): 2.4M ops/s (419ns/op)
- 读密集型: 14M ops/s (71ns/op)

**配置**:
```go
Storage: &gridkv.StorageOptions{
    Backend:     gridkv.BackendMemorySharded,
    ShardCount:  256,     // CPU 核心数的 2-4 倍
    MaxMemoryMB: 4096,
}
```

### Memory（简单）

**集群性能**:
- **5节点集群**: 1-2M ops/s (混合工作负载)
- **10节点集群**: 2-4M ops/s (混合工作负载)
- **读密集型 (10节点)**: 5-8M ops/s

**本地性能** (单节点):
- 小值 (100B): 933K ops/s (1.1µs/op)
- 中值 (1KB): 840K ops/s (1.2µs/op, 15% 压缩率)
- 读密集型: 2.2M ops/s (454ns/op)

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

**集群吞吐量** (MemorySharded 后端, QUIC 传输, LAN):

| 集群大小 | 混合工作负载 | 读密集型 | 写密集型 |
|----------|--------------|----------|----------|
| 3 节点 | 2-4M ops/s | 8-12M ops/s | 1-2M ops/s |
| 5 节点 | 3-6M ops/s | 15-25M ops/s | 2-3M ops/s |
| 10 节点 | 10-20M ops/s | 30-50M ops/s | 5-8M ops/s |
| 15 节点 | 15-30M ops/s | 45-75M ops/s | 8-12M ops/s |

**延迟**:
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
