package logging

import (
	"io"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	// GridKVPrefix is the log prefix for all GridKV messages
	GridKVPrefix = "[gridkv] "

	// Default log settings (constants to avoid allocations)
	DefaultLogLevel  = "info"
	DefaultLogFormat = "text"
)

var Log *Logger

func init() {
	Log = NewLogger(&LogOptions{
		Level:  DefaultLogLevel,
		Format: DefaultLogFormat,
	})
}

// LogOptions configures the behavior of the logging system.
type LogOptions struct {
	Level       string // Log level (e.g., "debug", "info", "warn", "error") default "info"
	Format      string // Format is the output format of the logs (e.g., "json", "text")
	EnableDebug bool   // Enable debug logging (zero-cost when false)
}

func Info(msg string, keyValues ...interface{}) {
	Log.LogInfo(msg, keyValues...)
}

func Debug(msg string, keyValues ...interface{}) {
	Log.LogDebug(msg, keyValues...)
}

func Warn(msg string, keyValues ...interface{}) {
	Log.LogWarn(msg, keyValues...)
}

func Error(err error, msg string, keyValues ...interface{}) {
	Log.LogError(err, msg, keyValues...)
}

func Fatal(err error, msg string, keyValues ...interface{}) {
	Log.LogFatal(err, msg, keyValues...)
}

// Logger is a wrapper around zerolog.Logger providing a high-performance,
// configuration-driven logging utility.
type Logger struct {
	logger zerolog.Logger
}

// NewLogger initializes and returns a new Logger instance based on the provided LogOptions.
// It configures the log level, output format (JSON/Console), and adds a timestamp.
func NewLogger(opts *LogOptions) *Logger {

	level, err := zerolog.ParseLevel(opts.Level)
	if err != nil {
		level = zerolog.InfoLevel
		log.Warn().Str("config_level", opts.Level).Msg("Invalid log level configured, defaulting to Info.")
	}

	// Set the global level. This is crucial for zerolog's performance optimization:
	// log calls below this level will be zero-allocated and skipped quickly.
	zerolog.SetGlobalLevel(level)

	var output io.Writer

	if opts.Format == "json" {
		output = os.Stdout
	} else {
		output = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: "2006-01-02 15:04:05", // Custom time format for readability
		}
	}

	// Add [gridkv] prefix to all log messages
	return &Logger{
		logger: zerolog.New(output).With().
			Timestamp().
			Str("prefix", "gridkv").
			Logger(),
	}
}

// LogDebug records a debugging message.
// ZERO-COST when disabled: This method is designed for zero-overhead in production.
// When global log level is Info or higher, this function returns immediately
// without any allocations or processing.
//
//go:inline
func (l Logger) LogDebug(msg string, keyValues ...interface{}) {
	// CRITICAL OPTIMIZATION: Early return if debug not enabled
	// This is completely free - just a level comparison
	if !l.logger.Debug().Enabled() {
		return
	}
	// Only execute expensive operations if debug is enabled
	l.logger.Debug().Fields(keyValues).Msg(msg)
}

// LogInfo records informational messages about normal application flow.
//
//go:inline
func (l Logger) LogInfo(msg string, keyValues ...interface{}) {
	if l.logger.Info().Enabled() {
		l.logger.Info().Fields(keyValues).Msg(msg)
	}
}

// IsDebugEnabled checks if debug logging is enabled (zero-cost check)
//
//go:inline
//go:nosplit
func (l Logger) IsDebugEnabled() bool {
	return l.logger.Debug().Enabled()
}

// IsInfoEnabled checks if info logging is enabled
//
//go:inline
//go:nosplit
func (l Logger) IsInfoEnabled() bool {
	return l.logger.Info().Enabled()
}

// LogWarn records messages about potential issues that do not immediately stop the application.
func (l Logger) LogWarn(msg string, keyValues ...interface{}) {
	l.logger.Warn().Fields(keyValues).Msg(msg)
}

// LogError records messages about failures, including an explicit error object.
func (l Logger) LogError(err error, msg string, keyValues ...interface{}) {
	// .Err(err) adds the error object to the structured log
	l.logger.Error().Err(err).Fields(keyValues).Msg(msg)
}

// LogFatal records a critical error and then exits the application with os.Exit(1).
func (l Logger) LogFatal(err error, msg string, keyValues ...interface{}) {
	l.logger.Fatal().Err(err).Fields(keyValues).Msg(msg)
}
