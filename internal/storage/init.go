package storage

func init() {
	// Auto-register Memory backend
	RegisterBackend(BackendMemory, func(opts *StorageOptions) (Storage, error) {
		maxMem := opts.MaxMemoryMB
		if maxMem == 0 {
			maxMem = 1024 // Default: 1GB
		}
		return NewMemoryStorage(maxMem)
	})

	// Auto-register MemorySharded backend V2 (optimized with configurable shards)
	RegisterBackend(BackendMemorySharded, func(opts *StorageOptions) (Storage, error) {
		maxMem := opts.MaxMemoryMB
		if maxMem == 0 {
			maxMem = 1024 // Default: 1GB
		}

		shardCount := opts.ShardCount
		if shardCount == 0 {
			shardCount = 256 // Default: 256 shards for optimal concurrency
		}

		return NewShardedMemoryStorageV2(V2Config{
			MaxMemoryMB: maxMem,
			ShardCount:  shardCount,
		})
	})
}
