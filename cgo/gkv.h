#ifndef GKV_H
#define GKV_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/**
 * Result structure for instance creation.
 * 
 * Memory management:
 * - Allocated by gkv_new(), must be freed with gkv_free_result()
 * - error_msg: NULL on success, otherwise points to error string (must be freed with gkv_free_string() if not NULL)
 */
typedef struct {
    uintptr_t instance_id;  // Instance ID for subsequent operations (0 if error)
    char* error_msg;         // Error message (NULL on success, must be freed if not NULL)
} gkv_result_t;

/**
 * Result structure for get operations.
 * 
 * Memory management:
 * - Allocated by gkv_get(), must be freed with gkv_free_get_result()
 * - data: Points to binary data (must be freed via gkv_free_get_result())
 * - data_len: Length of data in bytes
 * - error_msg: NULL on success, otherwise points to error string (must be freed via gkv_free_get_result())
 */
typedef struct {
    char* data;              // Binary data (NULL if not found or error)
    size_t data_len;         // Length of data in bytes (0 if data is NULL)
    char* error_msg;         // Error message (NULL on success, must be freed via gkv_free_get_result() if not NULL)
} gkv_get_result_t;

/**
 * Cluster statistics structure.
 */
typedef struct {
    int ready;               // 1 if cluster is ready, 0 otherwise
    int cluster_size;        // Total number of nodes in cluster
    int healthy_nodes;       // Number of healthy nodes
    int replica_factor;      // Replication factor
    char* local_node_id;     // Local node ID (must be freed via gkv_free_string())
    int pubkeys_ready;        // 1 if pubkeys are ready, 0 otherwise
    int pubkey_count;        // Number of pubkeys
    int peer_count;          // Number of peers
} gkv_cluster_stats_t;

/**
 * Network statistics structure.
 */
typedef struct {
    uint64_t server_connections;  // Server connections
    uint64_t server_messages;     // Server messages
    uint64_t server_bytes;        // Server bytes
    uint64_t server_errors;       // Server errors
    int64_t server_active_conns;  // Server active connections
    int64_t pool_total;           // Pool total connections
    int64_t pool_active;          // Pool active connections
    int64_t pool_idle;            // Pool idle connections
    int64_t pool_waiters;         // Pool waiters
    uint64_t pool_created;        // Pool created connections
    uint64_t pool_closed;         // Pool closed connections
    uint64_t pool_errors;         // Pool errors
    uint64_t client_requests;     // Client requests
    uint64_t client_responses;    // Client responses
    uint64_t client_errors;       // Client errors
    uint64_t client_bytes;        // Client bytes
} gkv_network_stats_t;

/**
 * Storage statistics structure.
 */
typedef struct {
    int64_t key_count;           // Number of keys
    int64_t total_bytes;         // Total bytes used
    int64_t compressed_bytes;    // Compressed bytes
    int64_t original_bytes;      // Original bytes before compression
    double compression_ratio;    // Compression ratio
    int64_t get_count;            // Get operation count
    int64_t set_count;            // Set operation count
    int64_t hit_count;            // Cache hit count
    int64_t miss_count;           // Cache miss count
    double hit_rate;              // Cache hit rate
    int64_t evict_count;          // Eviction count
} gkv_storage_stats_t;

/**
 * Complete statistics structure.
 * 
 * Memory management:
 * - Allocated by gkv_stats(), must be freed with gkv_free_stats()
 * - error_msg: NULL on success, otherwise points to error string (must be freed via gkv_free_string() if not NULL)
 * - local_node_id: Points to string (must be freed via gkv_free_stats())
 */
typedef struct {
    gkv_cluster_stats_t cluster;   // Cluster statistics
    gkv_network_stats_t network;   // Network statistics
    gkv_storage_stats_t storage;   // Storage statistics
    char* version;                  // Version string (must be freed via gkv_free_stats())
    char* error_msg;                // Error message (NULL on success, must be freed via gkv_free_stats() if not NULL)
} gkv_stats_t;

/**
 * Create new GridKV instance.
 * 
 * @param config_json JSON configuration string (UTF-8 encoded, null-terminated)
 * @return Pointer to gkv_result_t. Check error_msg field:
 *         - NULL: success, use instance_id for subsequent operations
 *         - Non-NULL: error occurred, contains error message
 * @note Caller must free result using gkv_free_result()
 * @note All strings are UTF-8 encoded
 */
gkv_result_t* gkv_new(const char* config_json);

/**
 * Set key-value pair.
 * 
 * @param instance_id Instance ID from gkv_new()
 * @param key Key string (UTF-8 encoded, null-terminated)
 * @param value Value data (can be binary, not null-terminated)
 * @param value_len Length of value data in bytes
 * @param ttl_sec TTL in seconds (0 = no expiration)
 * @return Error message string or NULL on success
 * @note Caller must free error message using gkv_free_string() if not NULL
 * @note value can contain null bytes (binary data supported)
 */
char* gkv_set(uintptr_t instance_id, const char* key, const char* value, size_t value_len, int ttl_sec);

/**
 * Get value by key.
 * 
 * @param instance_id Instance ID from gkv_new()
 * @param key Key string (UTF-8 encoded, null-terminated)
 * @return Pointer to gkv_get_result_t. Check error_msg field:
 *         - NULL: success, check data and data_len
 *         - Non-NULL: error occurred, contains error message
 * @note Caller must free result using gkv_free_get_result()
 * @note data may contain null bytes (binary data supported)
 */
gkv_get_result_t* gkv_get(uintptr_t instance_id, const char* key);

/**
 * Delete key.
 * 
 * @param instance_id Instance ID from gkv_new()
 * @param key Key string (UTF-8 encoded, null-terminated)
 * @return Error message string or NULL on success
 * @note Caller must free error message using gkv_free_string() if not NULL
 */
char* gkv_delete(uintptr_t instance_id, const char* key);

/**
 * Close instance and release resources.
 * 
 * @param instance_id Instance ID from gkv_new()
 * @param timeout_sec Shutdown timeout in seconds (0 = use default 30s)
 * @return Error message string or NULL on success
 * @note Caller must free error message using gkv_free_string() if not NULL
 * @note After calling this, instance_id becomes invalid
 */
char* gkv_close(uintptr_t instance_id, int timeout_sec);

/**
 * Free result memory allocated by gkv_new().
 * 
 * @param result Result pointer from gkv_new() (can be NULL)
 * @note Safe to call with NULL pointer
 * @note This also frees error_msg if present
 */
void gkv_free_result(gkv_result_t* result);

/**
 * Free get result memory allocated by gkv_get().
 * 
 * @param result Result pointer from gkv_get() (can be NULL)
 * @note Safe to call with NULL pointer
 * @note This frees data, error_msg, and result structure
 */
void gkv_free_get_result(gkv_get_result_t* result);

/**
 * Free string memory allocated by library functions.
 * 
 * @param str String pointer returned from library (can be NULL)
 * @note Safe to call with NULL pointer
 * @note Use this for error messages returned from gkv_set(), gkv_delete(), etc.
 */
void gkv_free_string(char* str);

/**
 * Get library version string.
 * 
 * @return Static version string (do NOT free)
 * @note This is a static string, never call free() on it
 */
const char* gkv_version(void);

/**
 * Health check.
 * 
 * @param instance_id Instance ID from gkv_new()
 * @return Error message string or NULL if healthy
 * @note Caller must free error message using gkv_free_string() if not NULL
 */
char* gkv_health_check(uintptr_t instance_id);

/**
 * Wait for cluster to be ready.
 * 
 * @param instance_id Instance ID from gkv_new()
 * @param timeout_sec Timeout in seconds (0 = use default 30s)
 * @return Error message string or NULL on success
 * @note Caller must free error message using gkv_free_string() if not NULL
 */
char* gkv_wait_ready(uintptr_t instance_id, int timeout_sec);

/**
 * Get statistics for GridKV instance.
 * 
 * @param instance_id Instance ID from gkv_new()
 * @return Pointer to gkv_stats_t. Check error_msg field:
 *         - NULL: success, use stats fields
 *         - Non-NULL: error occurred, contains error message
 * @note Caller must free result using gkv_free_stats()
 */
gkv_stats_t* gkv_stats(uintptr_t instance_id);

/**
 * Free statistics memory allocated by gkv_stats().
 * 
 * @param stats Stats pointer from gkv_stats() (can be NULL)
 * @note Safe to call with NULL pointer
 * @note This frees all strings (version, local_node_id, error_msg) and stats structure
 */
void gkv_free_stats(gkv_stats_t* stats);

/**
 * Check if result has error (helper function).
 * 
 * @param result Result pointer from gkv_new()
 * @return 1 if error exists, 0 if success
 * @note This is a convenience function, equivalent to checking result->error_msg != NULL
 */
int gkv_result_has_error(gkv_result_t* result);

/**
 * Check if get result has error (helper function).
 * 
 * @param result Result pointer from gkv_get()
 * @return 1 if error exists, 0 if success
 * @note This is a convenience function, equivalent to checking result->error_msg != NULL
 */
int gkv_get_result_has_error(gkv_get_result_t* result);

#ifdef __cplusplus
}
#endif

#endif // GKV_H
