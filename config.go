package gridkv

import (
	"io"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// Logging level constants
const (
	LogLevelDebug = logging.LevelDebug
	LogLevelInfo  = logging.LevelInfo
	LogLevelWarn  = logging.LevelWarn
	LogLevelError = logging.LevelError
	LogLevelFatal = logging.LevelFatal
)

// Logging format constants
const (
	LogFormatText    = logging.FormatText
	LogFormatJSON    = logging.FormatJSON
	LogFormatCompact = logging.FormatCompact
)

// NetworkType represents the transport protocol.
type NetworkType int

const (
	TCP  NetworkType = 1 // Reliable transport
	QUIC NetworkType = 3 // QUIC
)

// NetworkOptions configures networking.
type NetworkOptions struct {
	Type         NetworkType   // Transport protocol
	BindAddr     string        // Local bind address
	MaxIdle      int           // Max idle conns per peer
	MaxConns     int           // Max total conns per peer
	Timeout      time.Duration // Dial timeout
	ReadTimeout  time.Duration // Read timeout
	WriteTimeout time.Duration // Write timeout
}

// StorageOptions configures storage.
type StorageOptions struct {
	MaxMemoryMB int64
	ShardCount  int
}

// LoggerOptions configures logging.
type LoggerOptions struct {
	Level      string
	Format     string
	Output     io.Writer
	TimeFormat string
	NoCaller   bool
	NoTime     bool
}

// GridKVOptions configures a GridKV instance.
type GridKVOptions struct {
	LocalNodeID  string
	LocalAddress string
	SeedAddrs    []string

	TTL             time.Duration
	HotReadCacheTTL time.Duration
	VirtualNodes    int
	ReplicaCount    int

	ReadRepairRateLimitPerSec int64

	FailureTimeout     time.Duration
	SuspectTimeout     time.Duration
	GossipInterval     time.Duration
	ReplicationTimeout time.Duration
	ReadTimeout        time.Duration
	StartupGracePeriod time.Duration
	DisableAuth        bool

	Network *NetworkOptions
	Storage *StorageOptions

	Log interface{}
}

func applyDefaults(opts *GridKVOptions) {
	if opts == nil {
		return
	}

	// Defaults for network
	if opts.Network == nil {
		opts.Network = &NetworkOptions{}
	}
	if opts.Network.Type == 0 {
		opts.Network.Type = QUIC
	}
	if opts.Network.BindAddr == "" {
		opts.Network.BindAddr = opts.LocalAddress
	}
	if opts.Network.MaxConns == 0 {
		opts.Network.MaxConns = 128
	}
	if opts.Network.MaxIdle == 0 {
		opts.Network.MaxIdle = 32
	}
	if opts.Network.Timeout == 0 {
		opts.Network.Timeout = 10 * time.Second
	}
	if opts.Network.ReadTimeout == 0 {
		opts.Network.ReadTimeout = 5 * time.Second
	}
	if opts.Network.WriteTimeout == 0 {
		opts.Network.WriteTimeout = 5 * time.Second
	}

	// Defaults for storage
	if opts.Storage == nil {
		opts.Storage = &StorageOptions{}
	}
	if opts.Storage.MaxMemoryMB == 0 {
		opts.Storage.MaxMemoryMB = 1024
	}

	// Defaults for logging
	if opts.Log == nil {
		opts.Log = LoggerOptions{
			Level:  logging.LevelInfo,
			Format: logging.FormatText,
		}
	}

	// General defaults
	if opts.VirtualNodes == 0 {
		opts.VirtualNodes = 128
	}
	if opts.ReplicaCount == 0 {
		opts.ReplicaCount = 3
	}
	if opts.FailureTimeout == 0 {
		opts.FailureTimeout = 3 * time.Second
	}
	if opts.SuspectTimeout == 0 {
		opts.SuspectTimeout = opts.FailureTimeout / 2
	}
	if opts.GossipInterval == 0 {
		opts.GossipInterval = 200 * time.Millisecond
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 5 * time.Second
	}
	if opts.ReplicationTimeout == 0 {
		opts.ReplicationTimeout = 2 * time.Second
	}
	if opts.StartupGracePeriod == 0 {
		opts.StartupGracePeriod = 1 * time.Second
	}
}
