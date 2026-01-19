//go:build cgo
// +build cgo

package cgo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCInterface(t *testing.T) {
	cgoDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current directory: %v", err)
	}

	testDir := filepath.Join(cgoDir, "tests")
	testCFile := filepath.Join(testDir, "test_c.c")
	headerFile := filepath.Join(cgoDir, "gridkv_cgo.h")
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

	// Run the test runner script
	cmd := exec.Command("bash", testRunner)
	cmd.Dir = testDir
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=..")

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("C test failed: %v\nOutput:\n%s", err, string(output))
	}

	t.Logf("C test output:\n%s", string(output))
}
