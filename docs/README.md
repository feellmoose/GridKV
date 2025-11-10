# GridKV Documentation

Welcome to GridKV documentation!

---

## 📚 Quick Navigation

### Getting Started
- [Main README](../README.md) - Quick start and overview
- [Examples](../examples/) - Working code examples

### Core Concepts
- [Embedded Architecture](EMBEDDED_ARCHITECTURE.md) - Why embedded? ⭐NEW
- [Architecture](ARCHITECTURE.md) - System design
- [Consistency Model](CONSISTENCY_MODEL.md) - Quorum guarantees
- [Gossip Protocol](GOSSIP_PROTOCOL.md) - SWIM auto-clustering

### Getting Started
- [Quick Start](QUICK_START.md) - 5-minute tutorial ⭐NEW
- [API Reference](API_REFERENCE.md) - Complete API docs ⭐NEW
- [Deployment Guide](DEPLOYMENT_GUIDE.md) - Docker & K8s ⭐NEW

### Features
- [Feature List](FEATURES.md) - What's implemented ⭐NEW
- [Hybrid Logical Clock](HYBRID_LOGICAL_CLOCK.md) - Distributed timestamps
- [Storage Backends](STORAGE_BACKENDS.md) - Memory vs Sharded
- [Transport Layer](TRANSPORT_LAYER.md) - TCP networking
- [Metrics Export](METRICS_EXPORT.md) - Prometheus & OTLP

### Advanced
- [Performance Guide](PERFORMANCE.md) - Benchmarks and tuning

---

## 🎯 Documentation by Use Case

### I want to understand GridKV
→ Start with [Embedded Architecture](EMBEDDED_ARCHITECTURE.md)  
→ Read [Architecture](ARCHITECTURE.md)

### I want to deploy GridKV
→ Check [Main README](../README.md)  
→ See [Examples](../examples/)

### I want to optimize performance
→ Read [Performance Guide](PERFORMANCE.md)  
→ Check [Storage Backends](STORAGE_BACKENDS.md)

### I want to monitor GridKV
→ See [Metrics Export](METRICS_EXPORT.md)  
→ Check [examples/11_metrics_export](../examples/11_metrics_export/)

---

## 📖 Document Structure

```
docs/
├── README.md (this file)              - Documentation index
│
├── Getting Started
│   ├── QUICK_START.md                 - 5-minute tutorial ⭐NEW
│   ├── API_REFERENCE.md               - Complete API ⭐NEW
│   └── DEPLOYMENT_GUIDE.md            - Docker & K8s ⭐NEW
│
├── Core Concepts
│   ├── EMBEDDED_ARCHITECTURE.md       - Why embedded? ⭐NEW
│   ├── ARCHITECTURE.md                - System design
│   ├── CONSISTENCY_MODEL.md           - Quorum replication
│   └── GOSSIP_PROTOCOL.md             - SWIM protocol
│
├── Features
│   ├── FEATURES.md                    - What's implemented ⭐NEW
│   ├── HYBRID_LOGICAL_CLOCK.md        - HLC timestamps
│   ├── STORAGE_BACKENDS.md            - Storage options
│   ├── TRANSPORT_LAYER.md             - Network layer
│   └── METRICS_EXPORT.md              - Prometheus & OTLP
│
└── Advanced
    └── PERFORMANCE.md                 - Benchmarks & tuning
```

---

## 🚀 Quick Links

**New to GridKV?**  
→ [Main README](../README.md) - 5-minute quickstart

**Want to deploy?**  
→ [examples/05_production_ready](../examples/05_production_ready/)

**Need monitoring?**  
→ [Metrics Export](METRICS_EXPORT.md)

**Performance tuning?**  
→ [Performance Guide](PERFORMANCE.md)

---

**Last Updated**: 2025-11-09  
**GridKV Version**: v3.1
