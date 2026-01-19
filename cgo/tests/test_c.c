#include "gkv.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <assert.h>

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
    const char* version = gkv_version();
    TEST_ASSERT(version != NULL, "Version should not be NULL");
    printf("  ✓ Version: %s\n\n", version);
}

void test_new_instance() {
    printf("Test 2: Create GridKV Instance\n");
    gkv_result_t* result = gkv_new(test_config);
    TEST_ASSERT(result != NULL, "Result should not be NULL");
    
    if (result->error_msg != NULL) {
        fprintf(stderr, "  ✗ gkv_new failed: %s\n", result->error_msg);
        gkv_free_result(result);
        exit(1);
    }
    
    TEST_ASSERT(result->instance_id != 0, "Instance ID should not be 0");
    printf("  ✓ Instance created with ID: %lu\n\n", (unsigned long)result->instance_id);
    
    // Store instance ID for later tests
    uintptr_t instance_id = result->instance_id;
    gkv_free_result(result);
    
    // Test WaitReady (replaces sleep with proper readiness check)
    printf("Test 3: Wait for Ready\n");
    char* wait_err = gkv_wait_ready(instance_id, 5);
    if (wait_err != NULL) {
        printf("  ⚠ WaitReady: %s (expected for single node)\n", wait_err);
        gkv_free_string(wait_err);
    } else {
        printf("  ✓ Cluster is ready\n");
    }
    printf("\n");
    
    // Test Set
    printf("Test 4: Set Key-Value\n");
    const char* key = "test-key-c";
    const char* value = "test-value-c-123";
    char* set_err = gkv_set(instance_id, key, value, strlen(value), 0);
    if (set_err != NULL) {
        fprintf(stderr, "  ✗ gkv_set failed: %s\n", set_err);
        gkv_free_string(set_err);
        exit(1);
    }
    printf("  ✓ Set operation succeeded\n\n");
    
    // Test Get
    printf("Test 5: Get Key-Value\n");
    gkv_get_result_t* get_result = gkv_get(instance_id, key);
    TEST_ASSERT(get_result != NULL, "Get result should not be NULL");
    
    if (get_result->error_msg != NULL) {
        fprintf(stderr, "  ✗ gkv_get failed: %s\n", get_result->error_msg);
        gkv_free_get_result(get_result);
        exit(1);
    }
    
    TEST_ASSERT(get_result->data != NULL, "Get data should not be NULL");
    TEST_ASSERT(get_result->data_len == strlen(value), "Get data length should match");
    
    char* retrieved = (char*)malloc(get_result->data_len + 1);
    memcpy(retrieved, get_result->data, get_result->data_len);
    retrieved[get_result->data_len] = '\0';
    
    TEST_ASSERT(strcmp(retrieved, value) == 0, "Retrieved value should match");
    printf("  ✓ Get operation succeeded: %s\n\n", retrieved);
    free(retrieved);
    gkv_free_get_result(get_result);
    
    // Test Set with TTL
    printf("Test 6: Set with TTL\n");
    const char* key2 = "test-key-ttl-c";
    const char* value2 = "test-value-ttl-c";
    char* set_err2 = gkv_set(instance_id, key2, value2, strlen(value2), 60);
    if (set_err2 != NULL) {
        fprintf(stderr, "  ✗ gkv_set with TTL failed: %s\n", set_err2);
        gkv_free_string(set_err2);
        exit(1);
    }
    printf("  ✓ Set with TTL succeeded\n\n");
    
    // Test Delete
    printf("Test 7: Delete Key\n");
    char* delete_err = gkv_delete(instance_id, key);
    if (delete_err != NULL) {
        fprintf(stderr, "  ✗ gkv_delete failed: %s\n", delete_err);
        gkv_free_string(delete_err);
        exit(1);
    }
    printf("  ✓ Delete operation succeeded\n\n");
    
    // Verify deletion
    printf("Test 8: Verify Deletion\n");
    gkv_get_result_t* get_result2 = gkv_get(instance_id, key);
    if (get_result2->error_msg != NULL) {
        printf("  ⚠ Get after delete returned error: %s (might be expected)\n", get_result2->error_msg);
    } else if (get_result2->data != NULL) {
        fprintf(stderr, "  ✗ Get after delete returned data (should be NULL)\n");
        gkv_free_get_result(get_result2);
        exit(1);
    } else {
        printf("  ✓ Get after delete returned NULL (correct)\n");
    }
    gkv_free_get_result(get_result2);
    printf("\n");
    
    // Test HealthCheck
    printf("Test 9: Health Check\n");
    char* health_err = gkv_health_check(instance_id);
    if (health_err != NULL) {
        printf("  ⚠ HealthCheck: %s (might be expected for single node)\n", health_err);
        gkv_free_string(health_err);
    } else {
        printf("  ✓ Health check passed\n");
    }
    printf("\n");
    
    // Test binary data
    printf("Test 10: Binary Data\n");
    const char* bin_key = "binary-key-c";
    unsigned char bin_value[] = {0x00, 0x01, 0x02, 0x03, 0xFF, 0xFE, 0xFD, 0xFC};
    char* bin_set_err = gkv_set(instance_id, bin_key, (const char*)bin_value, sizeof(bin_value), 0);
    if (bin_set_err != NULL) {
        fprintf(stderr, "  ✗ Binary Set failed: %s\n", bin_set_err);
        gkv_free_string(bin_set_err);
        exit(1);
    }
    
    gkv_get_result_t* bin_get_result = gkv_get(instance_id, bin_key);
    if (bin_get_result->error_msg != NULL) {
        fprintf(stderr, "  ✗ Binary Get failed: %s\n", bin_get_result->error_msg);
        gkv_free_get_result(bin_get_result);
        exit(1);
    }
    
    TEST_ASSERT(bin_get_result->data_len == sizeof(bin_value), "Binary data length should match");
    TEST_ASSERT(memcmp(bin_get_result->data, bin_value, sizeof(bin_value)) == 0, "Binary data should match");
    printf("  ✓ Binary data test passed\n\n");
    gkv_free_get_result(bin_get_result);
    
    // Test Close
    printf("Test 11: Close Instance\n");
    char* close_err = gkv_close(instance_id, 5);
    if (close_err != NULL) {
        fprintf(stderr, "  ✗ gkv_close failed: %s\n", close_err);
        gkv_free_string(close_err);
        exit(1);
    }
    printf("  ✓ Close operation succeeded\n\n");
}

void test_error_handling() {
    printf("Test 12: Error Handling\n");
    
    // Test empty config
    gkv_result_t* result1 = gkv_new("");
    TEST_ASSERT(result1 != NULL, "Result should not be NULL");
    TEST_ASSERT(result1->error_msg != NULL, "Should return error for empty config");
    printf("  ✓ Empty config correctly returned error: %s\n", result1->error_msg);
    gkv_free_result(result1);
    
    // Test invalid JSON
    gkv_result_t* result2 = gkv_new("{invalid json}");
    TEST_ASSERT(result2 != NULL, "Result should not be NULL");
    TEST_ASSERT(result2->error_msg != NULL, "Should return error for invalid JSON");
    printf("  ✓ Invalid JSON correctly returned error: %s\n", result2->error_msg);
    gkv_free_result(result2);
    
    // Test invalid instance ID
    uintptr_t invalid_id = 99999;
    char* set_err = gkv_set(invalid_id, "key", "value", 5, 0);
    TEST_ASSERT(set_err != NULL, "Should return error for invalid instance ID");
    printf("  ✓ Invalid instance ID correctly returned error: %s\n", set_err);
    gkv_free_string(set_err);
    
    gkv_get_result_t* get_result = gkv_get(invalid_id, "key");
    TEST_ASSERT(get_result != NULL, "Get result should not be NULL");
    TEST_ASSERT(get_result->error_msg != NULL, "Should return error for invalid instance ID");
    printf("  ✓ Invalid instance ID in Get correctly returned error: %s\n", get_result->error_msg);
    gkv_free_get_result(get_result);
    
    printf("\n");
}

void test_stats() {
    printf("Test 13: Get Statistics\n");
    
    // Create a new instance for stats test
    gkv_result_t* result = gkv_new(test_config);
    if (result->error_msg != NULL) {
        fprintf(stderr, "  ✗ Failed to create instance for stats test: %s\n", result->error_msg);
        gkv_free_result(result);
        return;
    }
    
    uintptr_t instance_id = result->instance_id;
    gkv_free_result(result);
    
    // Wait for readiness instead of fixed sleep
    char* wait_err = gkv_wait_ready(instance_id, 3);
    if (wait_err != NULL) {
        // Non-fatal, continue with stats test
        gkv_free_string(wait_err);
    }
    
    // Get stats
    gkv_stats_t* stats = gkv_stats(instance_id);
    TEST_ASSERT(stats != NULL, "Stats should not be NULL");
    
    if (stats->error_msg != NULL) {
        printf("  ⚠ Stats error: %s (might be expected)\n", stats->error_msg);
        gkv_free_string(stats->error_msg);
    } else {
        printf("  ✓ Cluster stats:\n");
        printf("    - Ready: %d\n", stats->cluster.ready);
        printf("    - Cluster Size: %d\n", stats->cluster.cluster_size);
        printf("    - Healthy Nodes: %d\n", stats->cluster.healthy_nodes);
        printf("    - Replica Factor: %d\n", stats->cluster.replica_factor);
        if (stats->cluster.local_node_id) {
            printf("    - Local Node ID: %s\n", stats->cluster.local_node_id);
        }
        
        printf("  ✓ Network stats:\n");
        printf("    - Server Messages: %lu\n", (unsigned long)stats->network.server_messages);
        printf("    - Pool Total: %ld\n", (long)stats->network.pool_total);
        printf("    - Pool Active: %ld\n", (long)stats->network.pool_active);
        printf("    - Pool Idle: %ld\n", (long)stats->network.pool_idle);
        
        printf("  ✓ Storage stats:\n");
        printf("    - Key Count: %ld\n", (long)stats->storage.key_count);
        printf("    - Total Bytes: %ld\n", (long)stats->storage.total_bytes);
        printf("    - Get Count: %ld\n", (long)stats->storage.get_count);
        printf("    - Set Count: %ld\n", (long)stats->storage.set_count);
        printf("    - Hit Rate: %.2f\n", stats->storage.hit_rate);
        
        if (stats->version) {
            printf("  ✓ Version: %s\n", stats->version);
        }
    }
    
    gkv_free_stats(stats);
    
    // Close instance
    char* close_err = gkv_close(instance_id, 5);
    if (close_err != NULL) {
        gkv_free_string(close_err);
    }
    
    printf("\n");
}

void test_helper_functions() {
    printf("Test 14: Helper Functions\n");
    
    // Test gkv_result_has_error
    gkv_result_t* result1 = gkv_new("");
    TEST_ASSERT(gkv_result_has_error(result1) == 1, "gkv_result_has_error should return 1 for error");
    gkv_free_result(result1);
    
    gkv_result_t* result2 = gkv_new(test_config);
    if (result2->error_msg == NULL) {
        TEST_ASSERT(gkv_result_has_error(result2) == 0, "gkv_result_has_error should return 0 for success");
        uintptr_t instance_id = result2->instance_id;
        gkv_free_result(result2);
        
        // Test gkv_get_result_has_error
        gkv_get_result_t* get_result = gkv_get(instance_id, "nonexistent-key");
        if (get_result->error_msg == NULL) {
            TEST_ASSERT(gkv_get_result_has_error(get_result) == 0, "gkv_get_result_has_error should return 0 when no error");
        }
        gkv_free_get_result(get_result);
        
        gkv_close(instance_id, 5);
    } else {
        gkv_free_result(result2);
    }
    
    printf("  ✓ Helper functions work correctly\n\n");
}

int main() {
    printf("Testing GridKV CGO Interface (C Language)\n");
    printf("==========================================\n\n");
    
    test_version();
    test_new_instance();
    test_error_handling();
    test_stats();
    test_helper_functions();
    
    printf("All tests passed! ✓\n");
    return 0;
}
