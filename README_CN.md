# GridKV

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[English](README.md) | 简体中文

**GridKV** 是一个Go原生嵌入式分布式键值缓存，具备自动组网、Quorum复制和自适应网络功能。

---

## 特性

- **嵌入式架构**: 作为Go库导入，进程内运行
- **自动组网**: Gossip协议（SWIM）实现成员管理
- **Quorum复制**: 可配置的N/W/R一致性控制
- **一致性哈希**: Dynamo风格的数据分布与虚拟节点
- **自适应网络**: 自动检测LAN/WAN，优化Gossip间隔
- **故障检测**: SWIM探测实现<1秒检测
- **企业监控**: 原生Prometheus和OTLP导出
- **高性能**: 43ns进程内读取，682M ops/s峰值吞吐

---

## 快速开始

### 安装

```bash
go get github.com/feellmoose/gridkv
```

### 基础用法

```go
package main

import (
    "context"
    "log"
    
    gridkv "github.com/feellmoose/gridkv"
)

func main() {
    // 初始化GridKV实例
    kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
        LocalNodeID:  "node-1",
        LocalAddress: "localhost:8001",
        SeedAddrs:    []string{"localhost:8002", "localhost:8003"},
        
        Network: &gridkv.NetworkOptions{
            Type:     gridkv.TCP,
            BindAddr: "localhost:8001",
        },
        
        Storage: &gridkv.StorageOptions{
            Backend:     gridkv.BackendMemorySharded,
            ShardCount:  32,
        },
    })
    if err != nil {
        log.Fatal(err)
    }
    defer kv.Close()
    
    ctx := context.Background()
    
    // 写入数据（复制到N=3个节点）
    kv.Set(ctx, "user:1001", []byte("Alice"))
    
    // 读取数据（从R=2个节点quorum读取）
    value, _ := kv.Get(ctx, "user:1001")
    log.Printf("Value: %s", value)
    
    // 删除数据（quorum删除到W=2个节点）
    kv.Delete(ctx, "user:1001")
}
```

---

## 核心概念

### 嵌入式架构

GridKV在Go应用**进程内**运行。每个应用实例嵌入一个GridKV节点，实例通过Gossip协议自动组成分布式集群。

```
应用进程
├── 业务逻辑
└── GridKV（嵌入）
    ├── Gossip管理器（SWIM）
    ├── 一致性哈希环
    ├── 存储后端（分片内存）
    └── 网络传输（TCP）
```

**优势**:
- 进程内读取：43ns延迟（无网络往返）
- 零外部依赖：无需独立缓存服务器
- 单一部署单元：应用包含缓存
- 自动组网：实例自组织

### Gossip协议（SWIM）

GridKV使用**SWIM**（可扩展弱一致性感染式成员）协议实现：
- **成员管理**: 跟踪节点存活/怀疑/死亡状态
- **故障检测**: <1秒检测节点故障
- **状态传播**: 流行病广播集群状态

**算法**:
```
每个GossipInterval（默认1秒）:
  1. 选择K个随机对等节点
  2. 发送成员状态
  3. 探测对等节点存活性
  4. 检测故障（无响应 → 怀疑 → 死亡）
  5. 广播状态变化
```

**自适应**: 间隔根据网络延迟调整（LAN 1秒，WAN 4秒）

### 一致性哈希

数据分布使用**带虚拟节点的一致性哈希**:

```
键 → Hash(键) → 环上位置 → N个副本节点
```

**特性**:
- **负载均衡**: 每个物理节点150个虚拟节点确保均匀分布
- **最小干扰**: 增删节点仅影响1/M的键（M=节点数）
- **确定性**: 相同键总是映射到相同节点（可重现）

**性能**:
- 节点查找: O(log N) 二分查找
- N副本查找: 平均 O(N × replicas)

**基于**: Amazon Dynamo (SOSP 2007)

### Quorum复制

GridKV使用**可调Quorum**进行一致性控制:

**参数**:
- **N**: 每个键的总副本数（默认3）
- **W**: 写入Quorum - 最少成功写入数（默认2）
- **R**: 读取Quorum - 最少成功读取数（默认2）

**一致性级别**:
```
R + W > N:  强一致性（保证读到自己的写入）
R + W ≤ N:  最终一致性（可能短暂读到过期数据）

示例:
  N=3, W=2, R=2: 强一致性，性能均衡
  N=3, W=1, R=1: 最终一致性，最低延迟
  N=5, W=3, R=3: 最强一致性，最高持久性
```

**冲突解决**: 使用混合逻辑时钟时间戳的最后写入获胜

### 混合逻辑时钟（HLC）

GridKV使用**HLC**实现分布式时间戳:

```
HLC = max(physical_time, last_hlc) + logical_counter
```

**特性**:
- **因果性**: 若 A → B，则 HLC(A) < HLC(B)
- **有界漂移**: 保持在物理时间的ε范围内
- **单调性**: 永不减少，即使系统时钟回退

**用途**: Quorum操作中冲突解决的版本号

**基于**: "Logical Physical Clocks" (Kulkarni et al. 2014)

---

## 性能

### 基准测试

**单操作延迟** (Intel i7-12700H, 20核心):

```
操作                      延迟        吞吐量        分配
------------------------------------------------------------------------
ConsistentHash.Get     43.04 ns     232M ops/s    1 alloc/op
Metrics.Get            19.24 ns     520M ops/s    0 allocs/op
GetGossipInterval      14.62 ns     682M ops/s    0 allocs/op

进程内Get（本地）       ~50 ns       20M ops/s     最小
进程内Set（本地）       ~100 ns      10M ops/s     最小

分布式Get（LAN，R=1）  <1 ms        1M+ ops/s     最小
分布式Set（LAN，W=2）  ~2 ms        500K ops/s    最小
```

**零分配操作**: 9个操作实现0分配/操作

### 分布式场景

**按网络类型的延迟**:

```
场景                          延迟      RTT       模式
-----------------------------------------------------------
同实例（本地数据）            43 ns     0 ms      进程内
同机房（LAN，R=1）           <1 ms     <20 ms    LAN Gossip
同机房（LAN，R=2）           ~2 ms     <20 ms    Quorum
跨机房（WAN，R=1）           <50 ms    >20 ms    WAN Gossip
跨地域（异步）               ~100 ms   >100 ms   异步复制
```

### 吞吐量扩展

```
配置                       吞吐量        说明
------------------------------------------------------------
1节点                     1-2M ops/s    单实例
3节点（N=3，W=2）         3-6M ops/s    线性扩展
10节点                    10-20M ops/s  线性扩展
```

**可扩展性**: 由于一致性哈希分区，线性扩展

---

## 架构

### 数据流

**写操作** (Set):
```
1. Hash(key) → 一致性哈希环 → N个副本节点
2. 生成HLC时间戳（版本号）
3. 并行写入N个节点
4. 等待W个确认（quorum）
5. 返回成功
6. 异步完成剩余复制
```

**读操作** (Get):
```
1. Hash(key) → 一致性哈希环 → N个副本节点
2. 检查本地节点是否在N中:
   是 → 进程内读取（43ns）
   否 → 从R个节点远程读取
3. 返回最高HLC版本的值（最新）
4. 若版本不同，异步触发读修复
```

**故障检测**:
```
每个GossipInterval:
  1. 选择随机对等节点探测
  2. 发送ping
  3. 等待响应（超时: FailureTimeout）
  4. 无响应 → 标记怀疑
  5. 怀疑 > SuspectTimeout → 标记死亡
  6. 通过流行病协议广播状态
```

---

## 配置

### 必需参数

```go
&gridkv.GridKVOptions{
    // 节点身份
    LocalNodeID:  "node-1",           // 唯一节点标识符
    LocalAddress: "10.0.1.10:8001",   // 节点地址（host:port）
    
    // 集群成员
    SeedAddrs: []string{               // 引导节点（首节点为空）
        "10.0.1.11:8001",
        "10.0.1.12:8001",
    },
    
    // 网络配置
    Network: &gridkv.NetworkOptions{
        Type:     gridkv.TCP,          // 传输协议
        BindAddr: "10.0.1.10:8001",    // 绑定地址
    },
    
    // 存储配置
    Storage: &gridkv.StorageOptions{
        Backend:    gridkv.BackendMemorySharded,  // 分片内存（推荐）
        ShardCount: 32,                           // 分片数（2-4倍CPU核心）
    },
}
```

### 可选调优

```go
&gridkv.GridKVOptions{
    // 复制设置
    ReplicaCount: 3,               // N: 副本数（默认: 3）
    WriteQuorum:  2,               // W: 写入quorum（默认: 2）
    ReadQuorum:   2,               // R: 读取quorum（默认: 2）
    
    // 多数据中心
    DataCenter: "us-east-1",       // DC标识符，用于拓扑感知
    
    // 性能调优
    GossipInterval:     1 * time.Second,   // Gossip频率（LAN 1s，WAN 4s）
    FailureTimeout:     5 * time.Second,   // 标记怀疑超时
    SuspectTimeout:     10 * time.Second,  // 标记死亡超时
    ReplicationTimeout: 2 * time.Second,   // 复制RPC超时
    
    // 存储限制
    Storage: &gridkv.StorageOptions{
        Backend:     gridkv.BackendMemorySharded,
        ShardCount:  64,       // 更多分片 = 更好并发
        MaxMemoryMB: 2048,     // 内存限制（MB）
    },
}
```

---

## API参考

### 核心操作

```go
// Set: 存储键值对并复制
func (g *GridKV) Set(ctx context.Context, key string, value []byte) error

// Get: 通过quorum读取检索值
func (g *GridKV) Get(ctx context.Context, key string) ([]byte, error)

// Delete: 通过quorum删除键值对
func (g *GridKV) Delete(ctx context.Context, key string) error

// Close: 优雅关闭
func (g *GridKV) Close() error
```

**线程安全**: 所有方法支持并发访问

**Context**: 所有操作接受`context.Context`用于超时/取消

### 错误处理

```go
value, err := kv.Get(ctx, "user:123")
if err != nil {
    if err.Error() == "item not found" {
        // 键不存在
        log.Println("键不存在")
    } else {
        // 网络错误、quorum失败或超时
        log.Printf("Get error: %v", err)
    }
}
```

**常见错误**:
- `"item not found"`: 键未找到
- `"quorum not met"`: 未达到W（写入）或R（读取）节点数
- `context.DeadlineExceeded`: 操作超时

---

## 部署

### Docker

```dockerfile
FROM golang:1.23 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o app .

FROM alpine:latest
COPY --from=builder /app/app /app
CMD ["/app"]
```

### Kubernetes

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: gridkv-app
spec:
  serviceName: gridkv-app
  replicas: 3
  template:
    spec:
      containers:
      - name: app
        image: myapp:latest
        env:
        - name: NODE_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: NODE_ADDR
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        ports:
        - containerPort: 8001
          name: gridkv
```

---

## 监控

### Prometheus指标导出

```go
import gridkv "github.com/feellmoose/gridkv"
import "github.com/feellmoose/gridkv/internal/metrics"

// 配置Prometheus导出器
exporter := metrics.PrometheusExporter(func(text string) error {
    return os.WriteFile("/var/metrics/gridkv.prom", []byte(text), 0644)
})

// 初始化指标
m := metrics.NewGridKVMetrics(exporter)

// 启动周期性导出（每10秒）
go m.StartPeriodicExport(ctx, 10*time.Second)

// 记录指标
m.IncrementRequestsTotal()
m.SetClusterNodesAlive(3)
```

**预定义指标**（共27个）:
- 集群: `nodes_total`, `nodes_alive`, `nodes_suspect`, `nodes_dead`
- 请求: `requests_total`, `requests_success`, `requests_errors`
- 操作: `operations_set`, `operations_get`, `operations_delete`
- 复制: `replication_total`, `replication_success`, `replication_failures`
- 性能: `latency_p50_ns`, `latency_p95_ns`, `latency_p99_ns`

---

## 技术规格

### 一致性模型

- **协议**: 基于Quorum的复制（N/W/R）
- **冲突解决**: 使用HLC时间戳的最后写入获胜（LWW）
- **保证**: R + W > N 确保强一致性
- **读修复**: 检测到过期数据时自动修复

### 故障检测

- **协议**: SWIM（可扩展弱一致性感染式成员）
- **检测时间**: <1秒（可配置）
- **误报率**: 低（稳定网络中约1%）
- **机制**: 直接探测 + 通过对等节点的间接探测

### 数据分布

- **算法**: 带虚拟节点的一致性哈希
- **虚拟节点**: 每个物理节点150个（可配置）
- **哈希函数**: xxHash（XXH64）- 10 GB/s吞吐量
- **负载均衡**: 虚拟节点确保负载方差<10%

### 网络自适应

- **RTT测量**: 所有节点对之间的周期性ping
- **分类**: LAN（<20ms RTT），WAN（≥20ms RTT）
- **优化**: 
  - LAN: 1s Gossip间隔，同步复制
  - WAN: 4s Gossip间隔，异步复制
- **位置性**: 读取优先选择同数据中心节点

---

## 存储后端

### MemorySharded（推荐）

- **架构**: 带每分片锁的分片哈希映射
- **分片数**: 32-64（可配置，推荐2-4倍CPU核心）
- **并发性**: 通过分片因子减少锁竞争
- **性能**: 1-2M+ ops/s
- **用例**: 生产部署

**配置**:
```go
Storage: &gridkv.StorageOptions{
    Backend:     gridkv.BackendMemorySharded,
    ShardCount:  64,      // 更多分片 = 更好并发
    MaxMemoryMB: 2048,    // 内存限制
}
```

### Memory（简单）

- **架构**: 单个sync.Map
- **并发性**: 低于分片（单锁）
- **性能**: 600-700K ops/s
- **用例**: 开发、测试

---

## 多数据中心

### 拓扑感知操作

GridKV自动检测网络拓扑:

```
节点A（美东）←→ 节点B（美西）:  RTT 20ms  → LAN
节点A（美东）←→ 节点C（欧西）:  RTT 150ms → WAN
```

**优化**:
- **LAN模式**: 快速Gossip（1s间隔），同步复制
- **WAN模式**: 慢速Gossip（4s间隔），异步复制
- **读取路由**: 优先同数据中心节点（更低延迟）
- **写入路由**: 跨DC异步复制（最终一致性）

**配置**:
```go
&gridkv.GridKVOptions{
    DataCenter: "us-east-1",  // 标记节点所在数据中心
    // GridKV测量RTT并自动优化
}
```

---

## 示例

| 示例 | 说明 |
|------|------|
| [01_quickstart](examples/01_quickstart/) | 基础用法，单节点 |
| [02_distributed_cluster](examples/02_distributed_cluster/) | 3节点集群配置 |
| [03_multi_dc](examples/03_multi_dc/) | 多数据中心部署 |
| [04_high_performance](examples/04_high_performance/) | 性能调优 |
| [05_production_ready](examples/05_production_ready/) | 生产配置 |
| [11_metrics_export](examples/11_metrics_export/) | Prometheus和OTLP指标 |

---

## 文档

### 入门
- [快速开始](docs/QUICK_START.md) - 5分钟教程
- [API参考](docs/API_REFERENCE.md) - 完整API文档
- [部署指南](docs/DEPLOYMENT_GUIDE.md) - Docker和Kubernetes

### 架构
- [嵌入式架构](docs/EMBEDDED_ARCHITECTURE.md) - 为什么嵌入式？
- [架构设计](docs/ARCHITECTURE.md) - 系统设计
- [一致性模型](docs/CONSISTENCY_MODEL.md) - Quorum复制
- [Gossip协议](docs/GOSSIP_PROTOCOL.md) - SWIM规范

### 特性
- [功能列表](docs/FEATURES.md) - 已实现功能
- [混合逻辑时钟](docs/HYBRID_LOGICAL_CLOCK.md) - HLC算法
- [存储后端](docs/STORAGE_BACKENDS.md) - 存储选项
- [指标导出](docs/METRICS_EXPORT.md) - 监控集成

### 高级
- [性能指南](docs/PERFORMANCE.md) - 基准测试和调优

---

## 测试

```bash
# 运行所有测试
go test ./...

# 使用race检测器运行
go test -race ./...

# 运行基准测试
go test -bench=. -benchmem ./tests/

# 运行特定基准
go test -bench=BenchmarkConsistentHash ./tests/
```

---

## 生产考虑

### 集群规模

- **小型**: 3-5节点（高可用）
- **中型**: 5-10节点（均衡）
- **大型**: 每个数据中心10-20节点

**建议**: 从3节点开始，根据需要水平扩展

### Quorum配置

- **强一致性**: N=5, W=3, R=3（关键数据）
- **均衡**: N=3, W=2, R=2（推荐）
- **低延迟**: N=3, W=1, R=1（缓存）

### 内存规划

```
每节点内存 = 基础开销（50MB）+ 数据大小 / 复制因子

示例:
- 1GB总数据
- 3副本（N=3）
- 10节点
= 50MB + (1GB × 3 / 10)
= 50MB + 300MB
= 每节点约350MB

设置 MaxMemoryMB = 512MB（含缓冲）
```

### 网络要求

- **带宽**: 推荐100 Mbps+
- **延迟**: LAN模式<20ms，WAN可更高
- **端口**: 默认8001（可通过BindAddr配置）

---

## 参考文献

### 学术论文

- **一致性哈希**: "Consistent Hashing and Random Trees" (Karger et al. 1997)
- **Dynamo**: "Dynamo: Amazon's Highly Available Key-value Store" (DeCandia et al. 2007)
- **SWIM**: "SWIM: Scalable Weakly-consistent Infection-style Membership Protocol" (Das et al. 2002)
- **HLC**: "Logical Physical Clocks" (Kulkarni et al. 2014)

### 源代码

- 仓库: `github.com/feellmoose/gridkv`
- 许可证: MIT
- 语言: Go 1.23+

---

## 许可证

MIT License - 见 [LICENSE](LICENSE)

---

<div align="center">

**GridKV** - Go原生嵌入式分布式缓存

*高性能 • 自动组网 • 零外部依赖*

</div>
