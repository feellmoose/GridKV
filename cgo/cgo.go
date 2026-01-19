//go:build cgo
// +build cgo

package cgo

/*
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

typedef struct {
    uintptr_t instance;
    char* error;
} GridKVResult;

typedef struct {
    char* data;
    size_t len;
    char* error;
} GridKVGetResult;
*/
import "C"
import (
	"context"
	"encoding/json"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/feellmoose/gridkv"
)

var (
	instances     sync.Map
	instanceID    uintptr
	instanceMutex sync.Mutex
)

//export GridKVNew
func GridKVNew(configJSON *C.char) *C.GridKVResult {
	result := (*C.GridKVResult)(C.malloc(C.size_t(unsafe.Sizeof(C.GridKVResult{}))))
	result.instance = 0
	result.error = nil

	configStr := C.GoString(configJSON)
	if configStr == "" {
		result.error = C.CString("config JSON is required")
		return result
	}

	var cfg gridkv.Config
	if err := json.Unmarshal([]byte(configStr), &cfg); err != nil {
		result.error = C.CString(err.Error())
		return result
	}

	opts, err := gridkv.ParseConfig([]byte(configStr))
	if err != nil {
		result.error = C.CString(err.Error())
		return result
	}

	kv, err := gridkv.NewGridKV(opts)
	if err != nil {
		result.error = C.CString(err.Error())
		return result
	}

	instanceMutex.Lock()
	instanceID++
	id := instanceID
	instanceMutex.Unlock()

	instances.Store(id, kv)
	result.instance = C.uintptr_t(id)

	return result
}

//export GridKVSet
func GridKVSet(instanceID C.uintptr_t, key *C.char, value *C.char, valueLen C.size_t, ttlSeconds C.int) *C.char {
	id := uintptr(instanceID)
	kvInterface, ok := instances.Load(id)
	if !ok {
		return C.CString("instance not found")
	}

	kv, ok := kvInterface.(*gridkv.GridKV)
	if !ok {
		return C.CString("invalid instance type")
	}

	keyStr := C.GoString(key)
	if keyStr == "" {
		return C.CString("key cannot be empty")
	}

	var valueBytes []byte
	if valueLen > 0 {
		valueBytes = C.GoBytes(unsafe.Pointer(value), C.int(valueLen))
	}

	var ttl time.Duration
	if ttlSeconds > 0 {
		ttl = time.Duration(ttlSeconds) * time.Second
	}

	ctx := context.Background()
	if err := kv.Set(ctx, keyStr, valueBytes, ttl); err != nil {
		return C.CString(err.Error())
	}

	return nil
}

//export GridKVGet
func GridKVGet(instanceID C.uintptr_t, key *C.char) *C.GridKVGetResult {
	result := (*C.GridKVGetResult)(C.malloc(C.size_t(unsafe.Sizeof(C.GridKVGetResult{}))))
	result.data = nil
	result.len = 0
	result.error = nil

	id := uintptr(instanceID)
	kvInterface, ok := instances.Load(id)
	if !ok {
		result.error = C.CString("instance not found")
		return result
	}

	kv, ok := kvInterface.(*gridkv.GridKV)
	if !ok {
		result.error = C.CString("invalid instance type")
		return result
	}

	keyStr := C.GoString(key)
	if keyStr == "" {
		result.error = C.CString("key cannot be empty")
		return result
	}

	ctx := context.Background()
	value, err := kv.Get(ctx, keyStr)
	if err != nil {
		result.error = C.CString(err.Error())
		return result
	}

	if value == nil {
		return result
	}

	result.len = C.size_t(len(value))
	if result.len > 0 {
		result.data = (*C.char)(C.malloc(result.len))
		C.memcpy(unsafe.Pointer(result.data), unsafe.Pointer(&value[0]), result.len)
	}

	return result
}

//export GridKVDelete
func GridKVDelete(instanceID C.uintptr_t, key *C.char) *C.char {
	id := uintptr(instanceID)
	kvInterface, ok := instances.Load(id)
	if !ok {
		return C.CString("instance not found")
	}

	kv, ok := kvInterface.(*gridkv.GridKV)
	if !ok {
		return C.CString("invalid instance type")
	}

	keyStr := C.GoString(key)
	if keyStr == "" {
		return C.CString("key cannot be empty")
	}

	ctx := context.Background()
	if err := kv.Delete(ctx, keyStr); err != nil {
		return C.CString(err.Error())
	}

	return nil
}

//export GridKVClose
func GridKVClose(instanceID C.uintptr_t, timeoutSeconds C.int) *C.char {
	id := uintptr(instanceID)
	kvInterface, ok := instances.LoadAndDelete(id)
	if !ok {
		return C.CString("instance not found")
	}

	kv, ok := kvInterface.(*gridkv.GridKV)
	if !ok {
		return C.CString("invalid instance type")
	}

	var timeout time.Duration
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}

	if err := kv.Close(timeout); err != nil {
		return C.CString(err.Error())
	}

	runtime.GC()
	return nil
}

//export GridKVFreeResult
func GridKVFreeResult(result *C.GridKVResult) {
	if result == nil {
		return
	}
	if result.error != nil {
		C.free(unsafe.Pointer(result.error))
	}
	C.free(unsafe.Pointer(result))
}

//export GridKVFreeGetResult
func GridKVFreeGetResult(result *C.GridKVGetResult) {
	if result == nil {
		return
	}
	if result.data != nil {
		C.free(unsafe.Pointer(result.data))
	}
	if result.error != nil {
		C.free(unsafe.Pointer(result.error))
	}
	C.free(unsafe.Pointer(result))
}

//export GridKVFreeString
func GridKVFreeString(str *C.char) {
	if str != nil {
		C.free(unsafe.Pointer(str))
	}
}

//export GridKVVersion
func GridKVVersion() *C.char {
	return C.CString(gridkv.Version)
}

//export GridKVHealthCheck
func GridKVHealthCheck(instanceID C.uintptr_t) *C.char {
	id := uintptr(instanceID)
	kvInterface, ok := instances.Load(id)
	if !ok {
		return C.CString("instance not found")
	}

	kv, ok := kvInterface.(*gridkv.GridKV)
	if !ok {
		return C.CString("invalid instance type")
	}

	if err := kv.HealthCheck(); err != nil {
		return C.CString(err.Error())
	}

	return nil
}

//export GridKVWaitReady
func GridKVWaitReady(instanceID C.uintptr_t, timeoutSeconds C.int) *C.char {
	id := uintptr(instanceID)
	kvInterface, ok := instances.Load(id)
	if !ok {
		return C.CString("instance not found")
	}

	kv, ok := kvInterface.(*gridkv.GridKV)
	if !ok {
		return C.CString("invalid instance type")
	}

	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	if err := kv.WaitReady(timeout); err != nil {
		return C.CString(err.Error())
	}

	return nil
}
