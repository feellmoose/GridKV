//go:build cgo
// +build cgo

package cgo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCInterface(t *testing.T) {
	// Set timeout for test execution
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cgoDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current directory: %v", err)
	}

	testDir := filepath.Join(cgoDir, "tests")
	testCFile := filepath.Join(testDir, "test_c.c")
	headerFile := filepath.Join(cgoDir, "gkv.h")
	testRunner := filepath.Join(testDir, "test_runner.sh")

	// Check if files exist
	if _, err := os.Stat(testCFile); os.IsNotExist(err) {
		t.Skipf("C test file not found: %s", testCFile)
	}
	if _, err := os.Stat(headerFile); os.IsNotExist(err) {
		t.Fatalf("Header file not found: %s", headerFile)
	}
	if _, err := os.Stat(testRunner); os.IsNotExist(err) {
		t.Skipf("Test runner script not found: %s", testRunner)
	}

	// Run the test runner script with context
	cmd := exec.CommandContext(ctx, "bash", testRunner)
	cmd.Dir = testDir
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=..")

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Extract error information for better debugging
		outputStr := string(output)
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("C test timed out after 30s\nOutput:\n%s", outputStr)
		}
		t.Fatalf("C test failed: %v\nOutput:\n%s", err, outputStr)
	}

	// Parse output for test results
	outputStr := string(output)
	if strings.Contains(outputStr, "FAIL") {
		t.Errorf("C test reported failures:\n%s", outputStr)
	}
	if !strings.Contains(outputStr, "All tests passed") {
		t.Errorf("C test did not complete successfully:\n%s", outputStr)
	}

	t.Logf("C test completed successfully")
	if testing.Verbose() {
		t.Logf("C test output:\n%s", outputStr)
	}
}
