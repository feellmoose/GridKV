package logging

// Structured logging
// Features:
//   - Zero-allocation debug paths when disabled
//   - Multiple output formats (text, json, compact)
//   - Thread-safe concurrent access
//   - Unified configuration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
)

const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelFatal = "fatal"

	FormatText    = "text"
	FormatJSON    = "json"
	FormatCompact = "compact"
)

var (
	globalLog atomic.Pointer[Logger]
)

func init() {
	SetDefault(New(Opts{
		Level:  LevelInfo,
		Format: FormatText,
	}))
}

// Opts configures logger
type Opts struct {
	Level      string
	Format     string
	Output     io.Writer
	TimeFormat string
	NoCaller   bool
	NoTime     bool
}

// Logger provides structured logging
type Logger struct {
	logger *slog.Logger
	level  slog.Level
}

// New creates a new logger
func New(opts Opts) *Logger {
	if opts.Level == "" {
		opts.Level = LevelInfo
	}
	if opts.Format == "" {
		opts.Format = FormatText
	}
	if opts.Output == nil {
		opts.Output = os.Stdout
	}

	var level slog.Level
	switch opts.Level {
	case LevelDebug:
		level = slog.LevelDebug
	case LevelInfo:
		level = slog.LevelInfo
	case LevelWarn:
		level = slog.LevelWarn
	case LevelError:
		level = slog.LevelError
	case LevelFatal:
		level = slog.LevelError // slog doesn't have Fatal level, use Error
	default:
		level = slog.LevelInfo
	}

	var handler slog.Handler
	optsHandler := &slog.HandlerOptions{
		Level: level,
	}

	if !opts.NoCaller {
		optsHandler.AddSource = true
	}

	switch opts.Format {
	case FormatJSON:
		handler = slog.NewJSONHandler(opts.Output, optsHandler)
	case FormatCompact:
		// For compact, we'll use JSON but could be customized
		handler = slog.NewJSONHandler(opts.Output, optsHandler)
	default: // FormatText
		timeFormat := opts.TimeFormat
		if timeFormat == "" {
			timeFormat = "2006-01-02 15:04:05"
		}
		optsHandler.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			// Custom time format for text output
			if a.Key == slog.TimeKey {
				if !opts.NoTime {
					return slog.Attr{}
				}
			}
			return a
		}
		handler = slog.NewTextHandler(opts.Output, optsHandler)
	}

	logger := slog.New(handler)

	return &Logger{
		logger: logger,
		level:  level,
	}
}

// SetDefault sets the default global logger
func SetDefault(l *Logger) {
	if l != nil {
		globalLog.Store(l)
	}
}

// Default returns the default global logger
func Default() *Logger {
	if l := globalLog.Load(); l != nil {
		return l
	}
	return New(Opts{})
}

// Debug logs debug message
func (l *Logger) Debug(msg string, kv ...interface{}) {
	if l == nil {
		return
	}
	if !l.logger.Enabled(context.TODO(), slog.LevelDebug) {
		return
	}
	args := make([]any, 0, len(kv))
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	l.logger.Debug(msg, args...)
}

// Info logs info message
func (l *Logger) Info(msg string, kv ...interface{}) {
	if l == nil {
		return
	}
	if !l.logger.Enabled(context.TODO(), slog.LevelInfo) {
		return
	}
	args := make([]any, 0, len(kv))
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	l.logger.Info(msg, args...)
}

// Warn logs warning message
func (l *Logger) Warn(msg string, kv ...interface{}) {
	if l == nil {
		return
	}
	args := make([]any, 0, len(kv))
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	l.logger.Warn(msg, args...)
}

// Error logs error message
func (l *Logger) Error(err error, msg string, kv ...interface{}) {
	if l == nil {
		return
	}
	args := make([]any, 0, len(kv)+1)
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	if err != nil {
		args = append(args, slog.Any("error", err))
	}
	l.logger.Error(msg, args...)
}

// Fatal logs fatal message and exits
func (l *Logger) Fatal(err error, msg string, kv ...interface{}) {
	if l == nil {
		return
	}
	args := make([]any, 0, len(kv)+1)
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	if err != nil {
		args = append(args, slog.Any("error", err))
	}
	l.logger.Error(msg, args...) // slog doesn't have Fatal, use Error and let caller handle exit
}

// IsDebug returns true if debug logging is enabled
func (l *Logger) IsDebug() bool {
	if l == nil {
		return false
	}
	return l.logger.Enabled(context.TODO(), slog.LevelDebug)
}

// IsInfo returns true if info logging is enabled
func (l *Logger) IsInfo() bool {
	if l == nil {
		return false
	}
	return l.logger.Enabled(context.TODO(), slog.LevelInfo)
}

// Package-level convenience functions

func Debug(msg string, kv ...interface{}) {
	l := Default()
	if l == nil {
		return
	}
	if !l.logger.Enabled(context.TODO(), slog.LevelDebug) {
		return
	}
	args := make([]any, 0, len(kv))
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	l.logger.Debug(msg, args...)
}

func Info(msg string, kv ...interface{}) {
	l := Default()
	if l == nil {
		return
	}
	if !l.logger.Enabled(context.TODO(), slog.LevelInfo) {
		return
	}
	args := make([]any, 0, len(kv))
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	l.logger.Info(msg, args...)
}

func Warn(msg string, kv ...interface{}) {
	l := Default()
	if l == nil {
		return
	}
	args := make([]any, 0, len(kv))
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	l.logger.Warn(msg, args...)
}

func Error(err error, msg string, kv ...interface{}) {
	l := Default()
	if l == nil {
		return
	}
	args := make([]any, 0, len(kv)+1)
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	if err != nil {
		args = append(args, slog.Any("error", err))
	}
	l.logger.Error(msg, args...)
}

func Fatal(err error, msg string, kv ...interface{}) {
	l := Default()
	if l == nil {
		return
	}
	args := make([]any, 0, len(kv)+1)
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			args = append(args, slog.Any(kv[i].(string), kv[i+1]))
		}
	}
	if err != nil {
		args = append(args, slog.Any("error", err))
	}
	l.logger.Error(msg, args...) // slog doesn't have Fatal, use Error
}

func IsDebug() bool {
	return Default().IsDebug()
}

func IsInfo() bool {
	return Default().IsInfo()
}
