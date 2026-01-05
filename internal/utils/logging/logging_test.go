package logging

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestNew(t *testing.T) {
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
	})

	if logger == nil {
		t.Fatal("Expected non-nil logger")
	}
}

func TestNew_InvalidLevel(t *testing.T) {
	logger := New(Opts{
		Level:  "invalid",
		Format: FormatText,
	})

	if logger == nil {
		t.Fatal("Expected non-nil logger")
	}
}

func TestLogger_Info(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
	})

	logger.Info("test message", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "test message") {
		t.Fatalf("Expected log to contain 'test message', got: %s", output)
	}
}

func TestLogger_Debug_Enabled(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelDebug,
		Format: FormatText,
		Output: &buf,
	})

	logger.Debug("debug message", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "debug message") {
		t.Fatalf("Expected log to contain 'debug message', got: %s", output)
	}
}

func TestLogger_Debug_Disabled(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
	})

	logger.Debug("debug message", "key", "value")

	output := buf.String()
	if output != "" {
		t.Fatalf("Expected empty log when debug disabled, got: %s", output)
	}
}

func TestLogger_Warn(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelWarn,
		Format: FormatText,
		Output: &buf,
	})

	logger.Warn("warn message", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "warn message") {
		t.Fatalf("Expected log to contain 'warn message', got: %s", output)
	}
}

func TestLogger_Error(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelError,
		Format: FormatText,
		Output: &buf,
	})

	err := io.EOF
	logger.Error(err, "error message", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "error message") {
		t.Fatalf("Expected log to contain 'error message', got: %s", output)
	}
	if !strings.Contains(output, "EOF") {
		t.Fatalf("Expected log to contain error, got: %s", output)
	}
}

func TestLogger_Error_Nil(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelError,
		Format: FormatText,
		Output: &buf,
	})

	logger.Error(nil, "error message", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "error message") {
		t.Fatalf("Expected log to contain 'error message', got: %s", output)
	}
}

func TestLogger_IsDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelDebug,
		Format: FormatText,
		Output: &buf,
	})

	if !logger.IsDebug() {
		t.Fatal("Expected debug to be enabled")
	}

	// Create a new logger with Info level to test disabled debug
	loggerInfo := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
	})

	if loggerInfo.IsDebug() {
		t.Fatal("Expected debug to be disabled")
	}
}

func TestLogger_IsInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
	})

	if !logger.IsInfo() {
		t.Fatal("Expected info to be enabled")
	}

	// Create a new logger with Warn level to test disabled info
	loggerWarn := New(Opts{
		Level:  LevelWarn,
		Format: FormatText,
		Output: &buf,
	})

	if loggerWarn.IsInfo() {
		t.Fatal("Expected info to be disabled")
	}
}

func TestNew_JSONFormat(t *testing.T) {
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatJSON,
	})

	if logger == nil {
		t.Fatal("Expected non-nil logger")
	}
}

func TestNew_TextFormat(t *testing.T) {
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
	})

	if logger == nil {
		t.Fatal("Expected non-nil logger")
	}
}

func TestNew_CompactFormat(t *testing.T) {
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatCompact,
	})

	if logger == nil {
		t.Fatal("Expected non-nil logger")
	}
}

func TestNew_CustomOutput(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
	})

	logger.Info("test")
	if buf.Len() == 0 {
		t.Fatal("Expected log output")
	}
}

func TestNew_NoTime(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
		NoTime: true,
	})

	logger.Info("test")
	output := buf.String()
	if strings.Contains(output, "2006") || strings.Contains(output, "2024") {
		t.Fatal("Expected no timestamp in output")
	}
}

func TestNew_NoCaller(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:    LevelInfo,
		Format:   FormatText,
		Output:   &buf,
		NoCaller: true,
	})

	logger.Info("test")
	// Just verify it doesn't crash
}

func TestPackageFunctions(t *testing.T) {
	// Test package-level functions
	Info("test info", "key", "value")
	Warn("test warn", "key", "value")
	Error(io.EOF, "test error", "key", "value")
	Debug("test debug", "key", "value")
}

func TestLogger_Fatal(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelFatal,
		Format: FormatText,
		Output: &buf,
	})

	_ = logger.Fatal
}

func TestLogger_Concurrent(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
	})

	const numGoroutines = 10
	const logsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < logsPerGoroutine; j++ {
				logger.Info("concurrent log", "goroutine", id, "iteration", j)
			}
		}(i)
	}

	wg.Wait()
}

func TestLogger_MultipleKeyValues(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
	})

	logger.Info("test", "key1", "value1", "key2", "value2", "key3", "value3")

	output := buf.String()
	if !strings.Contains(output, "key1") || !strings.Contains(output, "value1") {
		t.Fatalf("Expected log to contain key1=value1, got: %s", output)
	}
}

func TestLogger_EmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
	})

	logger.Info("")

	output := buf.String()
	if output == "" {
		t.Fatal("Expected some log output even with empty message")
	}
}

func TestLogger_NoKeyValues(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: &buf,
	})

	logger.Info("test message")

	output := buf.String()
	if !strings.Contains(output, "test message") {
		t.Fatalf("Expected log to contain 'test message', got: %s", output)
	}
}

func TestLogger_Nil(t *testing.T) {
	var l *Logger
	l.Info("test") // Should not panic
	l.Debug("test")
	l.Warn("test")
	l.Error(nil, "test")
}

func TestSetDefault(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Opts{
		Level:  LevelDebug,
		Format: FormatText,
		Output: &buf,
	})

	SetDefault(logger)
	if Default() != logger {
		t.Fatal("Expected default logger to be set")
	}
}

func TestDefault(t *testing.T) {
	logger := Default()
	if logger == nil {
		t.Fatal("Expected non-nil default logger")
	}
}

func BenchmarkLogger_Info(b *testing.B) {
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: io.Discard,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.Info("benchmark message", "key", "value")
	}
}

func BenchmarkLogger_Debug_Disabled(b *testing.B) {
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
		Output: io.Discard,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.Debug("benchmark message", "key", "value")
	}
}

func BenchmarkLogger_IsDebug(b *testing.B) {
	logger := New(Opts{
		Level:  LevelDebug,
		Format: FormatText,
		Output: io.Discard,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = logger.IsDebug()
	}
}

func BenchmarkLogger_JSON(b *testing.B) {
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatJSON,
		Output: io.Discard,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.Info("benchmark message", "key", "value")
	}
}

func BenchmarkLogger_Compact(b *testing.B) {
	logger := New(Opts{
		Level:  LevelInfo,
		Format: FormatCompact,
		Output: io.Discard,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.Info("benchmark message", "key", "value")
	}
}
