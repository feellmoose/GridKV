# GridKV

[![Go Version](https://img.shields.io/badge/go-%3E%3D1.24-blue.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

高性能分布式键值缓存SDK，使用内存，具有最终一致性保证。

[English](./README.md)/中文

## 功能特性

- **高性能与并发安全**: 亚毫秒级本地操作针对并发工作负载优化，所有公共方法支持并发访问
- **最终一致性复制**: 流行病 gossip 协议配合批量复制确保节点间数据收敛
- **分布式容错架构**: 一致性哈希配合虚拟节点和 SWIM 协议实现负载均衡和故障检测
- **自适应网络通信**: LAN/WAN 环境优化，支持 TCP/QUIC 的节点间通信
- **内存存储**: 内存后端支持 TTL 和自动压缩

## 安装

```bash
go get github.com/feellmoose/gridkv
```

## 快速开始

```go
package main

import (
    "log"
    "time"
    "github.com/feellmoose/gridkv"
)

func main() {
    opts := &gridkv.GridKVOptions{
        LocalNodeID:  "node1",
        LocalAddress: "localhost:8080",
        SeedAddrs:    []string{"localhost:8081"},
    }

    kv, err := gridkv.NewGridKV(opts)
    if err != nil {
        log.Fatal(err)
    }
    defer kv.Close()

    // 设置键值对
    kv.Set("key", []byte("value"), time.Hour)

    // 获取值
    value, _ := kv.Get("key")
    println(string(value))
}
```

## 性能

运行基准测试：

```bash
go test -bench=. ./tests/
```

### 集成测试配置

集成测试和基准测试可以通过环境变量配置，以适应不同机器规格：

#### 内存配置
- `TEST_MAX_MEMORY_MB`: 默认每个节点的内存 (默认: 1024MB)
- `BENCH_MAX_MEMORY_MB`: 基准测试每个节点的内存 (默认: 256MB)
- `INTEGRATION_MAX_MEMORY_MB`: 集成测试每个节点的内存 (默认: 512MB)

#### 集群配置
- `TEST_NODE_COUNT`: 测试节点数量 (默认: 100)
- `BENCH_NODE_COUNT`: 基准测试节点数量 (默认: 50)
- `INTEGRATION_LARGE_CLUSTER_THRESHOLD`: 大集群阈值 (默认: 100)

#### 性能调优
- `INTEGRATION_CONCURRENCY`: 测试并发级别 (默认: 根据测试而异)
- `INTEGRATION_TEST_DURATION`: 测试持续时间 (默认: 5s)
- `BENCH_REPLICA_COUNT`: 基准测试复制因子 (默认: 3)

使用示例：
```bash
# 低内存机器 (8GB RAM)
export TEST_MAX_MEMORY_MB=256
export BENCH_MAX_MEMORY_MB=128
export TEST_NODE_COUNT=20

# 高性能机器 (64GB RAM)
export TEST_MAX_MEMORY_MB=4096
export BENCH_MAX_MEMORY_MB=2048
export TEST_NODE_COUNT=200
export INTEGRATION_CONCURRENCY=100

go test -v -short ./...
```

## 许可证

MIT License - 查看 [LICENSE](LICENSE) 获取详情
