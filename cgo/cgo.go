//go:build cgo
// +build cgo

package cgo

/*
#include <stdlib.h>
#include <stddef.h>
#include <stdint.h>
#include <stdbool.h>

typedef struct {
    uintptr_t instance_id;
    char* error_msg;
} gkv_result_t;

typedef struct {
    char* data;
    size_t data_len;
    char* error_msg;
} gkv_get_result_t;

typedef struct {
    int ready;
    int cluster_size;
    int healthy_nodes;
    int replica_factor;
    char* local_node_id;
    int pubkeys_ready;
    int pubkey_count;
    int peer_count;
} gkv_cluster_stats_t;

typedef struct {
    uint64_t server_connections;
    uint64_t server_messages;
    uint64_t server_bytes;
    uint64_t server_errors;
    int64_t server_active_conns;
    int64_t pool_total;
    int64_t pool_active;
    int64_t pool_idle;
    int64_t pool_waiters;
    uint64_t pool_created;
    uint64_t pool_closed;
    uint64_t pool_errors;
    uint64_t client_requests;
    uint64_t client_responses;
    uint64_t client_errors;
    uint64_t client_bytes;
} gkv_network_stats_t;

typedef struct {
    int64_t key_count;
    int64_t total_bytes;
    int64_t compressed_bytes;
    int64_t original_bytes;
    double compression_ratio;
    int64_t get_count;
    int64_t set_count;
    int64_t hit_count;
    int64_t miss_count;
    double hit_rate;
    int64_t evict_count;
} gkv_storage_stats_t;

typedef struct {
    gkv_cluster_stats_t cluster;
    gkv_network_stats_t network;
    gkv_storage_stats_t storage;
    char* version;
    char* error_msg;
} gkv_stats_t;
*/
import "C"
import (
	"context"
	"errors"
	"sync"
	"time"
	"unsafe"

	"github.com/feellmoose/gridkv"
)

var (
	instances     sync.Map
	instanceID    uintptr
	instanceMutex sync.Mutex
	ctx           = context.Background()
	versionStr    *C.char
	versionOnce   sync.Once
)

func getInstance(id uintptr) (*gridkv.GridKV, error) {
	kvInterface, ok := instances.Load(id)
	if !ok {
		return nil, errors.New("instance not found")
	}

	kv, ok := kvInterface.(*gridkv.GridKV)
	if !ok {
		return nil, errors.New("invalid instance type")
	}

	return kv, nil
}

//export gkv_new
func gkv_new(configJSON *C.char) *C.gkv_result_t {
	result := (*C.gkv_result_t)(C.malloc(C.size_t(unsafe.Sizeof(C.gkv_result_t{}))))
	result.instance_id = 0
	result.error_msg = nil

	if configJSON == nil {
		result.error_msg = C.CString("config JSON is required")
		return result
	}

	configStr := C.GoString(configJSON)
	if configStr == "" {
		result.error_msg = C.CString("config JSON is required")
		return result
	}

	opts, err := gridkv.ParseConfig([]byte(configStr))
	if err != nil {
		result.error_msg = C.CString(err.Error())
		return result
	}

	kv, err := gridkv.NewGridKV(opts)
	if err != nil {
		result.error_msg = C.CString(err.Error())
		return result
	}

	instanceMutex.Lock()
	instanceID++
	id := instanceID
	instanceMutex.Unlock()

	instances.Store(id, kv)
	result.instance_id = C.uintptr_t(id)

	return result
}

//export gkv_set
func gkv_set(instanceID C.uintptr_t, key *C.char, value *C.char, valueLen C.size_t, ttlSec C.int) *C.char {
	kv, err := getInstance(uintptr(instanceID))
	if err != nil {
		return C.CString(err.Error())
	}

	keyStr := C.GoString(key)
	if keyStr == "" {
		return C.CString("key cannot be empty")
	}

	var valueBytes []byte
	if valueLen > 0 && value != nil {
		valueBytes = C.GoBytes(unsafe.Pointer(value), C.int(valueLen))
	}

	var ttl time.Duration
	if ttlSec > 0 {
		ttl = time.Duration(ttlSec) * time.Second
	}

	if err := kv.Set(ctx, keyStr, valueBytes, ttl); err != nil {
		return C.CString(err.Error())
	}

	return nil
}

//export gkv_get
func gkv_get(instanceID C.uintptr_t, key *C.char) *C.gkv_get_result_t {
	result := (*C.gkv_get_result_t)(C.malloc(C.size_t(unsafe.Sizeof(C.gkv_get_result_t{}))))
	result.data = nil
	result.data_len = 0
	result.error_msg = nil

	kv, err := getInstance(uintptr(instanceID))
	if err != nil {
		result.error_msg = C.CString(err.Error())
		return result
	}

	keyStr := C.GoString(key)
	if keyStr == "" {
		result.error_msg = C.CString("key cannot be empty")
		return result
	}

	value, err := kv.Get(ctx, keyStr)
	if err != nil {
		result.error_msg = C.CString(err.Error())
		return result
	}

	if len(value) == 0 {
		return result
	}

	result.data_len = C.size_t(len(value))
	result.data = (*C.char)(C.CBytes(value))

	return result
}

//export gkv_delete
func gkv_delete(instanceID C.uintptr_t, key *C.char) *C.char {
	kv, err := getInstance(uintptr(instanceID))
	if err != nil {
		return C.CString(err.Error())
	}

	keyStr := C.GoString(key)
	if keyStr == "" {
		return C.CString("key cannot be empty")
	}

	if err := kv.Delete(ctx, keyStr); err != nil {
		return C.CString(err.Error())
	}

	return nil
}

//export gkv_close
func gkv_close(instanceID C.uintptr_t, timeoutSec C.int) *C.char {
	id := uintptr(instanceID)
	kvInterface, ok := instances.LoadAndDelete(id)
	if !ok {
		return C.CString("instance not found")
	}

	kv, ok := kvInterface.(*gridkv.GridKV)
	if !ok {
		return C.CString("invalid instance type")
	}

	timeout := 30 * time.Second
	if timeoutSec > 0 {
		timeout = time.Duration(timeoutSec) * time.Second
	}

	if err := kv.Close(timeout); err != nil {
		return C.CString(err.Error())
	}

	return nil
}

//export gkv_free_result
func gkv_free_result(result *C.gkv_result_t) {
	if result == nil {
		return
	}
	if result.error_msg != nil {
		C.free(unsafe.Pointer(result.error_msg))
	}
	C.free(unsafe.Pointer(result))
}

//export gkv_free_get_result
func gkv_free_get_result(result *C.gkv_get_result_t) {
	if result == nil {
		return
	}
	if result.data != nil {
		C.free(unsafe.Pointer(result.data))
	}
	if result.error_msg != nil {
		C.free(unsafe.Pointer(result.error_msg))
	}
	C.free(unsafe.Pointer(result))
}

//export gkv_free_string
func gkv_free_string(str *C.char) {
	if str != nil {
		C.free(unsafe.Pointer(str))
	}
}

//export gkv_version
func gkv_version() *C.char {
	versionOnce.Do(func() {
		versionStr = C.CString(gridkv.Version)
	})
	return versionStr
}

//export gkv_health_check
func gkv_health_check(instanceID C.uintptr_t) *C.char {
	kv, err := getInstance(uintptr(instanceID))
	if err != nil {
		return C.CString(err.Error())
	}

	if err := kv.HealthCheck(); err != nil {
		return C.CString(err.Error())
	}

	return nil
}

//export gkv_wait_ready
func gkv_wait_ready(instanceID C.uintptr_t, timeoutSec C.int) *C.char {
	kv, err := getInstance(uintptr(instanceID))
	if err != nil {
		return C.CString(err.Error())
	}

	timeout := time.Duration(timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	if err := kv.WaitReady(timeout); err != nil {
		return C.CString(err.Error())
	}

	return nil
}

//export gkv_stats
func gkv_stats(instanceID C.uintptr_t) *C.gkv_stats_t {
	result := (*C.gkv_stats_t)(C.malloc(C.size_t(unsafe.Sizeof(C.gkv_stats_t{}))))
	*result = C.gkv_stats_t{}

	kv, err := getInstance(uintptr(instanceID))
	if err != nil {
		result.error_msg = C.CString(err.Error())
		return result
	}

	stats := kv.Stats()

	cluster := &result.cluster
	cluster.ready = C.int(boolToInt(stats.Cluster.Ready))
	cluster.cluster_size = C.int(stats.Cluster.ClusterSize)
	cluster.healthy_nodes = C.int(stats.Cluster.HealthyNodes)
	cluster.replica_factor = C.int(stats.Cluster.ReplicaFactor)
	cluster.local_node_id = C.CString(stats.Cluster.LocalNodeID)
	cluster.pubkeys_ready = C.int(boolToInt(stats.Cluster.PubkeysReady))
	cluster.pubkey_count = C.int(stats.Cluster.PubkeyCount)
	cluster.peer_count = C.int(stats.Cluster.PeerCount)

	network := &result.network
	network.server_connections = C.uint64_t(stats.Network.ServerConnections)
	network.server_messages = C.uint64_t(stats.Network.ServerMessages)
	network.server_bytes = C.uint64_t(stats.Network.ServerBytes)
	network.server_errors = C.uint64_t(stats.Network.ServerErrors)
	network.server_active_conns = C.int64_t(stats.Network.ServerActiveConns)
	network.pool_total = C.int64_t(stats.Network.PoolTotal)
	network.pool_active = C.int64_t(stats.Network.PoolActive)
	network.pool_idle = C.int64_t(stats.Network.PoolIdle)
	network.pool_waiters = C.int64_t(stats.Network.PoolWaiters)
	network.pool_created = C.uint64_t(stats.Network.PoolCreated)
	network.pool_closed = C.uint64_t(stats.Network.PoolClosed)
	network.pool_errors = C.uint64_t(stats.Network.PoolErrors)
	network.client_requests = C.uint64_t(stats.Network.ClientRequests)
	network.client_responses = C.uint64_t(stats.Network.ClientResponses)
	network.client_errors = C.uint64_t(stats.Network.ClientErrors)
	network.client_bytes = C.uint64_t(stats.Network.ClientBytes)

	storage := &result.storage
	storage.key_count = C.int64_t(stats.Storage.KeyCount)
	storage.total_bytes = C.int64_t(stats.Storage.TotalBytes)
	storage.compressed_bytes = C.int64_t(stats.Storage.CompressedBytes)
	storage.original_bytes = C.int64_t(stats.Storage.OriginalBytes)
	storage.compression_ratio = C.double(stats.Storage.CompressionRatio)
	storage.get_count = C.int64_t(stats.Storage.GetCount)
	storage.set_count = C.int64_t(stats.Storage.SetCount)
	storage.hit_count = C.int64_t(stats.Storage.HitCount)
	storage.miss_count = C.int64_t(stats.Storage.MissCount)
	storage.hit_rate = C.double(stats.Storage.HitRate)
	storage.evict_count = C.int64_t(stats.Storage.EvictCount)

	result.version = gkv_version()

	return result
}

//export gkv_free_stats
func gkv_free_stats(stats *C.gkv_stats_t) {
	if stats == nil {
		return
	}
	if stats.cluster.local_node_id != nil {
		C.free(unsafe.Pointer(stats.cluster.local_node_id))
	}
	if stats.error_msg != nil {
		C.free(unsafe.Pointer(stats.error_msg))
	}
	C.free(unsafe.Pointer(stats))
}

//export gkv_result_has_error
func gkv_result_has_error(result *C.gkv_result_t) C.int {
	if result == nil {
		return 1
	}
	if result.error_msg != nil {
		return 1
	}
	return 0
}

//export gkv_get_result_has_error
func gkv_get_result_has_error(result *C.gkv_get_result_t) C.int {
	if result == nil {
		return 1
	}
	if result.error_msg != nil {
		return 1
	}
	return 0
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
