# GridKV

[![Go Version](https://img.shields.io/badge/go-%3E%3D1.24-blue.svg)](https://golang.org/)
[![GitHub License](https://img.shields.io/github/license/feellmoose/GridKV)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/feellmoose/gridkv)](https://goreportcard.com/report/github.com/feellmoose/gridkv)
[![GitHub Tag](https://img.shields.io/github/v/tag/feellmoose/GridKV)]()

高性能分布式键值缓存SDK，使用内存，具有最终一致性保证。

[English](./README.md) / 中文

## 功能特性

- **高性能与并发安全**: 亚毫秒级本地操作，所有公共方法支持并发访问
- **最终一致性复制**: Gossip协议配合批量复制确保节点间数据收敛
- **分布式容错架构**: 一致性哈希配合虚拟节点和 SWIM 协议实现负载均衡和故障检测
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

## 测试

```bash
# 运行所有测试
go test ./...

# 运行短测试
go test -short ./...

# 运行基准测试
go test -bench=. ./tests/
```

## 架构

- **SWIM协议**: 成员管理和故障检测
- **一致性哈希**: 跨节点键分布
- **Gossip协议**: 高效数据复制
- **HLC (混合逻辑时钟)**: 因果关系跟踪和冲突解决

查看 [internal/cluster/README.md](internal/cluster/README.md) 获取详细架构文档。

## 许可证

MIT License - 查看 [LICENSE](LICENSE) 获取详情
