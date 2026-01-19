#ifndef GRIDKV_CGO_H
#define GRIDKV_CGO_H

#include <stdlib.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct {
    uintptr_t instance;
    char* error;
} GridKVResult;

typedef struct {
    char* data;
    size_t len;
    char* error;
} GridKVGetResult;

// Create a new GridKV instance from JSON config
// Returns GridKVResult with instance ID or error message
// Caller must free result using GridKVFreeResult
GridKVResult* GridKVNew(const char* configJSON);

// Set a key-value pair
// instanceID: instance ID returned from GridKVNew
// key: key string (null-terminated)
// value: value data (can be binary)
// valueLen: length of value data
// ttlSeconds: TTL in seconds (0 = no expiration)
// Returns error message or NULL on success
// Caller must free error string using GridKVFreeString if not NULL
char* GridKVSet(uintptr_t instanceID, const char* key, const char* value, size_t valueLen, int ttlSeconds);

// Get a value by key
// instanceID: instance ID returned from GridKVNew
// key: key string (null-terminated)
// Returns GridKVGetResult with data or error message
// Caller must free result using GridKVFreeGetResult
GridKVGetResult* GridKVGet(uintptr_t instanceID, const char* key);

// Delete a key
// instanceID: instance ID returned from GridKVNew
// key: key string (null-terminated)
// Returns error message or NULL on success
// Caller must free error string using GridKVFreeString if not NULL
char* GridKVDelete(uintptr_t instanceID, const char* key);

// Close and cleanup GridKV instance
// instanceID: instance ID returned from GridKVNew
// timeoutSeconds: shutdown timeout in seconds (0 = use default 30s)
// Returns error message or NULL on success
// Caller must free error string using GridKVFreeString if not NULL
char* GridKVClose(uintptr_t instanceID, int timeoutSeconds);

// Free GridKVResult memory
void GridKVFreeResult(GridKVResult* result);

// Free GridKVGetResult memory
void GridKVFreeGetResult(GridKVGetResult* result);

// Free string returned from CGO functions
void GridKVFreeString(char* str);

// Get GridKV version string
// Returns version string (do not free, static string)
const char* GridKVVersion(void);

// Health check
// instanceID: instance ID returned from GridKVNew
// Returns error message or NULL if healthy
// Caller must free error string using GridKVFreeString if not NULL
char* GridKVHealthCheck(uintptr_t instanceID);

// Wait for cluster to be ready
// instanceID: instance ID returned from GridKVNew
// timeoutSeconds: timeout in seconds (0 = use default 30s)
// Returns error message or NULL on success
// Caller must free error string using GridKVFreeString if not NULL
char* GridKVWaitReady(uintptr_t instanceID, int timeoutSeconds);

#ifdef __cplusplus
}
#endif

#endif // GRIDKV_CGO_H
