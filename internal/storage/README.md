# Storage Package Structure

GridKV 的存储层实现，提供多种存储后端以适应不同场景。

---

## 📁 文件组织

### 核心接口与类型 (3 files)

**storage.go** (136 lines)
- 存储接口定义 (`Storage`, `HighPerformanceStorage`)
- 基础类型 (`StoredItem`, `CacheSyncOperation`, etc.)
- Backend 类型常量

**errors.go** (18 lines)
- 预分配的错误对象（减少分配）
- 常见错误定义

**registry.go** (78 lines)
- Backend 注册机制
- 工厂模式实现

### 存储后端实现 (2 files)

**memory.go** (~900 lines)
- Memory backend 完整实现
- 自动压缩 (50-70% 内存节省)
- LRU 驱逐
- 高性能API (GetNoCopy, BatchGet/Set)
- 结构清晰，按功能分区

**memory_sharded.go** (~730 lines)
- MemorySharded backend 完整实现
- 256 分片并发优化
- 极致性能 (2.9M Get ops/s)
- 高性能API (GetNoCopy, BatchGet/Set)
- 结构清晰，按功能分区

### 优化工具 (3 files)

**object_pool.go** (200 lines)
- sync.Pool 对象池
- StoredItem, CacheSyncOperation 池化
- Byte buffer 池

**optimizations.go** (230 lines)
- ValueBufferPool (多种size)
- HotKeyCache (热点缓存)
- GC 优化工具
- 系统配置推荐

**unsafe_utils.go** (90 lines)
- unsafe 优化工具
- StringToBytes (零分配)
- BytesToString (零分配)
- FastCloneBytes

### 监控与工具 (3 files)

**metrics.go** (184 lines)
- 性能监控
- QPS, 延迟, 错误率追踪
- MetricsSnapshot

**gossip_sync.go** (25 lines)
- Gossip 同步扩展接口

**init.go** (30 lines)
- Package 初始化
- Backend 自动注册

---

## 🎯 Backend 特性对比

| 文件 | Backend | 定位 | 性能 | 特点 |
|------|---------|------|------|------|
| `memory.go` | Memory | 轻量级+压缩 | 2.08M Get ops/s | 压缩、LRU |
| `memory_sharded.go` | MemorySharded | 极致性能 | 2.83M Get ops/s | 256分片、无压缩 |

---

## 🚀 高性能API

两个 backend 都实现了 `HighPerformanceStorage` 接口：

```go
type HighPerformanceStorage interface {
    Storage  // 基础接口
    
    GetNoCopy(key string) (*StoredItem, error)
    BatchGet(keys []string) (map[string]*StoredItem, error)
    BatchGetNoCopy(keys []string) (map[string]*StoredItem, error)
    BatchSet(items map[string]*StoredItem) error
}
```

**使用方式**: GridKV 内部自动检测和使用，对用户透明。

---

## 📖 代码可读性设计

### 文件头注释
每个文件都有清晰的头注释，说明：
- 文件目的
- 实现的 backend
- 代码结构（行号范围）
- 主要优化点

### 功能分区
每个实现文件内部按功能分区：
1. 类型定义
2. 构造函数
3. 核心 API
4. 高性能 API
5. Gossip 同步
6. 统计与工具

### 命名规范
- 接口: `Storage`, `HighPerformanceStorage`
- 实现: `MemoryStorage`, `ShardedMemoryStorage`
- 工具: `*Pool`, `*Cache`, `*Utils`
- 错误: `Err*`, `err*`

---

## 🔧 优化技术

### 已应用优化

| 优化技术 | 文件 | 收益 |
|---------|------|------|
| 预分配 error | errors.go | -1 alloc/op |
| unsafe 优化 | unsafe_utils.go | -1-2 allocs/op |
| 对象池 | object_pool.go | -3-5 allocs/op |
| FastCloneBytes | unsafe_utils.go | 更快的复制 |
| GC 调优 | optimizations.go | -10-20% GC |

### 性能提升

优化前后对比：
- Get: 352.8ns → 346.4ns (**+1.8%**)
- Set: 1348ns → 1277ns (**+5.6%**)
- 分配: -1 alloc/op (error对象)

---

## 📚 使用指南

### 选择 Backend

```go
// Memory - 轻量级+压缩
Storage: &storage.StorageOptions{
    Backend: storage.BackendMemory,
    MaxMemoryMB: 512,
}

// MemorySharded - 极致性能
Storage: &storage.StorageOptions{
    Backend: storage.BackendMemorySharded,
    MaxMemoryMB: 2048,
    ShardCount: 256,  // 可选
}
```

详见: `STORAGE_BACKEND_GUIDE.md`

---

**文件结构版本**: V2  
**最后更新**: 2025-11-07  
**总行数**: ~2,862 lines  
**文件数**: 11 files (清晰、模块化)

