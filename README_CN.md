# GridKV

[![Go Version](https://img.shields.io/badge/go-%3E%3D1.21-blue.svg)](https://golang.org/)
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

## 许可证

MIT License - 查看 [LICENSE](LICENSE) 获取详情
