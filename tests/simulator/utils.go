// Package simulator provides utility functions for GridKV testing.
// This file contains utility functions for testing.
package simulator

import (
	"net"
	"os"
	"strconv"
	"time"
)

// GetEnvInt parses integer environment variable with default fallback
func GetEnvInt(key string, defaultValue int) int {
	if val := os.Getenv(key); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultValue
}

// GetEnvInt64 parses environment variable as int64 with default fallback
func GetEnvInt64(key string, defaultValue int64) int64 {
	if val := os.Getenv(key); val != "" {
		if parsed, err := strconv.ParseInt(val, 10, 64); err == nil {
			return parsed
		}
	}
	return defaultValue
}

// GetEnvDuration parses duration environment variable with default fallback
func GetEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if val := os.Getenv(key); val != "" {
		if parsed, err := time.ParseDuration(val); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultValue
}

// GetFreePort returns a free port on localhost
func GetFreePort() (int, error) {
	addr, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer addr.Close()
	return addr.Addr().(*net.TCPAddr).Port, nil
}