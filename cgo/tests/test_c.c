#include "gridkv_cgo.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <assert.h>
#include <unistd.h>

#define TEST_ASSERT(cond, msg) \
    do { \
        if (!(cond)) { \
            fprintf(stderr, "FAIL: %s\n", msg); \
            exit(1); \
        } \
    } while (0)

static const char* test_config = 
    "{"
    "\"local_node_id\":\"test-node-c\","
    "\"local_address\":\"127.0.0.1:19010\","
    "\"seed_addrs\":[],"
    "\"virtual_nodes\":64,"
    "\"replica_count\":1,"
    "\"network\":{\"type\":\"tcp\"},"
    "\"storage\":{\"max_memory_mb\":256,\"shard_count\":8},"
    "\"log\":{\"level\":\"error\",\"format\":\"text\",\"no_caller\":true}"
    "}";

void test_version() {
    printf("Test 1: Get Version\n");
    const char* version = GridKVVersion();
    TEST_ASSERT(version != NULL, "Version should not be NULL");
    printf("  ✓ Version: %s\n\n", version);
}

void test_new_instance() {
    printf("Test 2: Create GridKV Instance\n");
    GridKVResult* result = GridKVNew(test_config);
    TEST_ASSERT(result != NULL, "Result should not be NULL");
    
    if (result->error != NULL) {
        fprintf(stderr, "  ✗ GridKVNew failed: %s\n", result->error);
        GridKVFreeResult(result);
        exit(1);
    }
    
    TEST_ASSERT(result->instance != 0, "Instance ID should not be 0");
    printf("  ✓ Instance created with ID: %lu\n\n", (unsigned long)result->instance);
    
    // Store instance ID for later tests
    uintptr_t instance_id = result->instance;
    GridKVFreeResult(result);
    
    // Wait a bit for initialization
    sleep(1); // 1 second
    
    // Test WaitReady
    printf("Test 3: Wait for Ready\n");
    char* wait_err = GridKVWaitReady(instance_id, 5);
    if (wait_err != NULL) {
        printf("  ⚠ WaitReady: %s (expected for single node)\n", wait_err);
        GridKVFreeString(wait_err);
    } else {
        printf("  ✓ Cluster is ready\n");
    }
    printf("\n");
    
    // Test Set
    printf("Test 4: Set Key-Value\n");
    const char* key = "test-key-c";
    const char* value = "test-value-c-123";
    char* set_err = GridKVSet(instance_id, key, value, strlen(value), 0);
    if (set_err != NULL) {
        fprintf(stderr, "  ✗ GridKVSet failed: %s\n", set_err);
        GridKVFreeString(set_err);
        exit(1);
    }
    printf("  ✓ Set operation succeeded\n\n");
    
    // Test Get
    printf("Test 5: Get Key-Value\n");
    GridKVGetResult* get_result = GridKVGet(instance_id, key);
    TEST_ASSERT(get_result != NULL, "Get result should not be NULL");
    
    if (get_result->error != NULL) {
        fprintf(stderr, "  ✗ GridKVGet failed: %s\n", get_result->error);
        GridKVFreeGetResult(get_result);
        exit(1);
    }
    
    TEST_ASSERT(get_result->data != NULL, "Get data should not be NULL");
    TEST_ASSERT(get_result->len == strlen(value), "Get data length should match");
    
    char* retrieved = (char*)malloc(get_result->len + 1);
    memcpy(retrieved, get_result->data, get_result->len);
    retrieved[get_result->len] = '\0';
    
    TEST_ASSERT(strcmp(retrieved, value) == 0, "Retrieved value should match");
    printf("  ✓ Get operation succeeded: %s\n\n", retrieved);
    free(retrieved);
    GridKVFreeGetResult(get_result);
    
    // Test Set with TTL
    printf("Test 6: Set with TTL\n");
    const char* key2 = "test-key-ttl-c";
    const char* value2 = "test-value-ttl-c";
    char* set_err2 = GridKVSet(instance_id, key2, value2, strlen(value2), 60);
    if (set_err2 != NULL) {
        fprintf(stderr, "  ✗ GridKVSet with TTL failed: %s\n", set_err2);
        GridKVFreeString(set_err2);
        exit(1);
    }
    printf("  ✓ Set with TTL succeeded\n\n");
    
    // Test Delete
    printf("Test 7: Delete Key\n");
    char* delete_err = GridKVDelete(instance_id, key);
    if (delete_err != NULL) {
        fprintf(stderr, "  ✗ GridKVDelete failed: %s\n", delete_err);
        GridKVFreeString(delete_err);
        exit(1);
    }
    printf("  ✓ Delete operation succeeded\n\n");
    
    // Verify deletion
    printf("Test 8: Verify Deletion\n");
    GridKVGetResult* get_result2 = GridKVGet(instance_id, key);
    if (get_result2->error != NULL) {
        printf("  ⚠ Get after delete returned error: %s (might be expected)\n", get_result2->error);
    } else if (get_result2->data != NULL) {
        fprintf(stderr, "  ✗ Get after delete returned data (should be NULL)\n");
        GridKVFreeGetResult(get_result2);
        exit(1);
    } else {
        printf("  ✓ Get after delete returned NULL (correct)\n");
    }
    GridKVFreeGetResult(get_result2);
    printf("\n");
    
    // Test HealthCheck
    printf("Test 9: Health Check\n");
    char* health_err = GridKVHealthCheck(instance_id);
    if (health_err != NULL) {
        printf("  ⚠ HealthCheck: %s (might be expected for single node)\n", health_err);
        GridKVFreeString(health_err);
    } else {
        printf("  ✓ Health check passed\n");
    }
    printf("\n");
    
    // Test binary data
    printf("Test 10: Binary Data\n");
    const char* bin_key = "binary-key-c";
    unsigned char bin_value[] = {0x00, 0x01, 0x02, 0x03, 0xFF, 0xFE, 0xFD, 0xFC};
    char* bin_set_err = GridKVSet(instance_id, bin_key, (const char*)bin_value, sizeof(bin_value), 0);
    if (bin_set_err != NULL) {
        fprintf(stderr, "  ✗ Binary Set failed: %s\n", bin_set_err);
        GridKVFreeString(bin_set_err);
        exit(1);
    }
    
    GridKVGetResult* bin_get_result = GridKVGet(instance_id, bin_key);
    if (bin_get_result->error != NULL) {
        fprintf(stderr, "  ✗ Binary Get failed: %s\n", bin_get_result->error);
        GridKVFreeGetResult(bin_get_result);
        exit(1);
    }
    
    TEST_ASSERT(bin_get_result->len == sizeof(bin_value), "Binary data length should match");
    TEST_ASSERT(memcmp(bin_get_result->data, bin_value, sizeof(bin_value)) == 0, "Binary data should match");
    printf("  ✓ Binary data test passed\n\n");
    GridKVFreeGetResult(bin_get_result);
    
    // Test Close
    printf("Test 11: Close Instance\n");
    char* close_err = GridKVClose(instance_id, 5);
    if (close_err != NULL) {
        fprintf(stderr, "  ✗ GridKVClose failed: %s\n", close_err);
        GridKVFreeString(close_err);
        exit(1);
    }
    printf("  ✓ Close operation succeeded\n\n");
}

void test_error_handling() {
    printf("Test 12: Error Handling\n");
    
    // Test empty config
    GridKVResult* result1 = GridKVNew("");
    TEST_ASSERT(result1 != NULL, "Result should not be NULL");
    TEST_ASSERT(result1->error != NULL, "Should return error for empty config");
    printf("  ✓ Empty config correctly returned error: %s\n", result1->error);
    GridKVFreeResult(result1);
    
    // Test invalid JSON
    GridKVResult* result2 = GridKVNew("{invalid json}");
    TEST_ASSERT(result2 != NULL, "Result should not be NULL");
    TEST_ASSERT(result2->error != NULL, "Should return error for invalid JSON");
    printf("  ✓ Invalid JSON correctly returned error: %s\n", result2->error);
    GridKVFreeResult(result2);
    
    // Test invalid instance ID
    uintptr_t invalid_id = 99999;
    char* set_err = GridKVSet(invalid_id, "key", "value", 5, 0);
    TEST_ASSERT(set_err != NULL, "Should return error for invalid instance ID");
    printf("  ✓ Invalid instance ID correctly returned error: %s\n", set_err);
    GridKVFreeString(set_err);
    
    GridKVGetResult* get_result = GridKVGet(invalid_id, "key");
    TEST_ASSERT(get_result != NULL, "Get result should not be NULL");
    TEST_ASSERT(get_result->error != NULL, "Should return error for invalid instance ID");
    printf("  ✓ Invalid instance ID in Get correctly returned error: %s\n", get_result->error);
    GridKVFreeGetResult(get_result);
    
    printf("\n");
}

int main() {
    printf("Testing GridKV CGO Interface (C Language)\n");
    printf("==========================================\n\n");
    
    test_version();
    test_new_instance();
    test_error_handling();
    
    printf("All tests passed! ✓\n");
    return 0;
}
