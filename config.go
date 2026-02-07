package gridkv

import (
	"encoding/json"
	"fmt"
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

// Config is used for JSON/YAML configuration file parsing with snake_case field names.
type Config struct {
	LocalNodeID  string   `json:"local_node_id"`
	LocalAddress string   `json:"local_address"`
	SeedAddrs    []string `json:"seed_addrs,omitempty"`

	TTL             string `json:"ttl,omitempty"` // Duration string (e.g., "5m")
	HotReadCacheTTL string `json:"hot_read_cache_ttl,omitempty"`
	HotCacheSize    int    `json:"hot_cache_size,omitempty"`
	VirtualNodes    int    `json:"virtual_nodes,omitempty"`
	ReplicaCount    int    `json:"replica_count,omitempty"`

	ReadRepairRateLimitPerSec int64 `json:"read_repair_rate_limit_per_sec,omitempty"`

	FailureTimeout     string `json:"failure_timeout,omitempty"`
	SuspectTimeout     string `json:"suspect_timeout,omitempty"`
	GossipInterval     string `json:"gossip_interval,omitempty"`
	ReplicationTimeout string `json:"replication_timeout,omitempty"`
	ReadTimeout        string `json:"read_timeout,omitempty"`
	StartupGracePeriod string `json:"startup_grace_period,omitempty"`
	DisableAuth        bool   `json:"disable_auth,omitempty"`

	BatchThreshold int    `json:"batch_threshold,omitempty"`
	BatchWindow    string `json:"batch_window,omitempty"`

	Network *NetworkConfig `json:"network,omitempty"`
	Storage *StorageConfig `json:"storage,omitempty"`
	Log     *LogConfig     `json:"log,omitempty"`
}

// NetworkConfig is used for JSON/YAML configuration file parsing.
type NetworkConfig struct {
	Type         string `json:"type,omitempty"` // "tcp" or "quic"
	BindAddr     string `json:"bind_addr,omitempty"`
	MaxIdle      int    `json:"max_idle,omitempty"`
	MaxConns     int    `json:"max_conns,omitempty"`
	Timeout      string `json:"timeout,omitempty"`       // Duration string
	ReadTimeout  string `json:"read_timeout,omitempty"`  // Duration string
	WriteTimeout string `json:"write_timeout,omitempty"` // Duration string
}

// StorageConfig is used for JSON/YAML configuration file parsing.
type StorageConfig struct {
	MaxMemoryMB int64 `json:"max_memory_mb,omitempty"`
	ShardCount  int   `json:"shard_count,omitempty"`
}

// LogConfig is used for JSON/YAML configuration file parsing.
type LogConfig struct {
	Level      string `json:"level,omitempty"`
	Format     string `json:"format,omitempty"`
	TimeFormat string `json:"time_format,omitempty"`
	NoCaller   bool   `json:"no_caller,omitempty"`
	NoTime     bool   `json:"no_time,omitempty"`
}

// Options is the internal configuration structure used for GridKV initialization.
type Options struct {
	LocalNodeID  string
	LocalAddress string
	SeedAddrs    []string

	TTL             time.Duration
	HotReadCacheTTL time.Duration
	HotCacheSize    int
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

	BatchThreshold int
	BatchWindow    time.Duration

	Network *NetworkOptions
	Storage *StorageOptions
	Log     interface{} // LoggerOptions, logging.Opts, or *logging.Logger
}

// NetworkOptions configures networking (internal use).
type NetworkOptions struct {
	Type         NetworkType
	BindAddr     string
	MaxIdle      int
	MaxConns     int
	Timeout      time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// StorageOptions configures storage (internal use).
type StorageOptions struct {
	MaxMemoryMB int64
	ShardCount  int
}

// LoggerOptions configures logging (internal use).
type LoggerOptions struct {
	Level      string
	Format     string
	Output     io.Writer
	TimeFormat string
	NoCaller   bool
	NoTime     bool
}

// Option is a functional option for configuring GridKV.
type Option func(*Options)

// GridKVOptions is deprecated, use Options or functional options instead.
// Kept for backward compatibility.
type GridKVOptions = Options

// ParseConfig parses a JSON configuration with snake_case field names into Options.
func ParseConfig(data []byte) (*Options, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	options := &Options{
		LocalNodeID:  cfg.LocalNodeID,
		LocalAddress: cfg.LocalAddress,
		SeedAddrs:    cfg.SeedAddrs,
		VirtualNodes: cfg.VirtualNodes,
		ReplicaCount: cfg.ReplicaCount,

		ReadRepairRateLimitPerSec: cfg.ReadRepairRateLimitPerSec,
		DisableAuth:               cfg.DisableAuth,
		BatchThreshold:            cfg.BatchThreshold,
		HotCacheSize:              10000, // Default cache size
	}

	if cfg.TTL != "" {
		d, err := time.ParseDuration(cfg.TTL)
		if err != nil {
			return nil, fmt.Errorf("invalid ttl: %w", err)
		}
		options.TTL = d
	}
	if cfg.HotReadCacheTTL != "" {
		d, err := time.ParseDuration(cfg.HotReadCacheTTL)
		if err != nil {
			return nil, fmt.Errorf("invalid hot_read_cache_ttl: %w", err)
		}
		options.HotReadCacheTTL = d
	}
	if cfg.HotCacheSize > 0 {
		options.HotCacheSize = cfg.HotCacheSize
	}
	if cfg.FailureTimeout != "" {
		d, err := time.ParseDuration(cfg.FailureTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid failure_timeout: %w", err)
		}
		options.FailureTimeout = d
	}
	if cfg.SuspectTimeout != "" {
		d, err := time.ParseDuration(cfg.SuspectTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid suspect_timeout: %w", err)
		}
		options.SuspectTimeout = d
	}
	if cfg.GossipInterval != "" {
		d, err := time.ParseDuration(cfg.GossipInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid gossip_interval: %w", err)
		}
		options.GossipInterval = d
	}
	if cfg.ReplicationTimeout != "" {
		d, err := time.ParseDuration(cfg.ReplicationTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid replication_timeout: %w", err)
		}
		options.ReplicationTimeout = d
	}
	if cfg.ReadTimeout != "" {
		d, err := time.ParseDuration(cfg.ReadTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid read_timeout: %w", err)
		}
		options.ReadTimeout = d
	}
	if cfg.StartupGracePeriod != "" {
		d, err := time.ParseDuration(cfg.StartupGracePeriod)
		if err != nil {
			return nil, fmt.Errorf("invalid startup_grace_period: %w", err)
		}
		options.StartupGracePeriod = d
	}
	if cfg.BatchWindow != "" {
		d, err := time.ParseDuration(cfg.BatchWindow)
		if err != nil {
			return nil, fmt.Errorf("invalid batch_window: %w", err)
		}
		options.BatchWindow = d
	}

	if cfg.Network != nil {
		options.Network = &NetworkOptions{
			BindAddr: cfg.Network.BindAddr,
			MaxIdle:  cfg.Network.MaxIdle,
			MaxConns: cfg.Network.MaxConns,
		}

		switch cfg.Network.Type {
		case "quic", "QUIC":
			options.Network.Type = QUIC
		default:
			options.Network.Type = TCP
		}

		if cfg.Network.Timeout != "" {
			d, err := time.ParseDuration(cfg.Network.Timeout)
			if err != nil {
				return nil, fmt.Errorf("invalid network timeout: %w", err)
			}
			options.Network.Timeout = d
		}
		if cfg.Network.ReadTimeout != "" {
			d, err := time.ParseDuration(cfg.Network.ReadTimeout)
			if err != nil {
				return nil, fmt.Errorf("invalid network read_timeout: %w", err)
			}
			options.Network.ReadTimeout = d
		}
		if cfg.Network.WriteTimeout != "" {
			d, err := time.ParseDuration(cfg.Network.WriteTimeout)
			if err != nil {
				return nil, fmt.Errorf("invalid network write_timeout: %w", err)
			}
			options.Network.WriteTimeout = d
		}
	}

	if cfg.Storage != nil {
		options.Storage = &StorageOptions{
			MaxMemoryMB: cfg.Storage.MaxMemoryMB,
			ShardCount:  cfg.Storage.ShardCount,
		}
	}

	if cfg.Log != nil {
		options.Log = LoggerOptions{
			Level:      cfg.Log.Level,
			Format:     cfg.Log.Format,
			TimeFormat: cfg.Log.TimeFormat,
			NoCaller:   cfg.Log.NoCaller,
			NoTime:     cfg.Log.NoTime,
		}
	}

	applyDefaults(options)
	return options, nil
}

func applyDefaults(options *Options) {
	if options == nil {
		return
	}

	if options.Network == nil {
		options.Network = &NetworkOptions{}
	}
	if options.Network.Type == 0 {
		options.Network.Type = TCP
	}
	if options.Network.BindAddr == "" {
		options.Network.BindAddr = options.LocalAddress
	}
	if options.Network.MaxConns == 0 {
		options.Network.MaxConns = 128
	}
	if options.Network.MaxIdle == 0 {
		options.Network.MaxIdle = 32
	}
	if options.Network.Timeout == 0 {
		options.Network.Timeout = 10 * time.Second
	}
	if options.Network.ReadTimeout == 0 {
		options.Network.ReadTimeout = 5 * time.Second
	}
	if options.Network.WriteTimeout == 0 {
		options.Network.WriteTimeout = 5 * time.Second
	}

	if options.Storage == nil {
		options.Storage = &StorageOptions{}
	}
	if options.Storage.MaxMemoryMB == 0 {
		options.Storage.MaxMemoryMB = 1024
	}

	if options.Log == nil {
		options.Log = LoggerOptions{
			Level:    logging.LevelInfo,
			Format:   logging.FormatText,
			NoCaller: true,
		}
	}

	if options.VirtualNodes == 0 {
		options.VirtualNodes = 128
	}
	if options.ReplicaCount == 0 {
		options.ReplicaCount = 3
	}
	if options.FailureTimeout == 0 {
		options.FailureTimeout = 5 * time.Second
	}
	if options.SuspectTimeout == 0 {
		options.SuspectTimeout = options.FailureTimeout / 2
	}
	if options.GossipInterval == 0 {
		options.GossipInterval = 200 * time.Millisecond
	}
	if options.ReadTimeout == 0 {
		options.ReadTimeout = 5 * time.Second
	}
	if options.ReplicationTimeout == 0 {
		options.ReplicationTimeout = 2 * time.Second
	}
	if options.BatchThreshold == 0 {
		options.BatchThreshold = 10
	}
	if options.BatchWindow == 0 {
		options.BatchWindow = 10 * time.Millisecond
	}
	if options.StartupGracePeriod == 0 {
		options.StartupGracePeriod = 1 * time.Second
	}
}

// Functional Options

// WithLocalNodeID sets the local node ID.
func WithLocalNodeID(nodeID string) Option {
	return func(o *Options) {
		o.LocalNodeID = nodeID
	}
}

// WithLocalAddress sets the local address.
func WithLocalAddress(addr string) Option {
	return func(o *Options) {
		o.LocalAddress = addr
	}
}

// WithSeedAddrs sets the seed addresses.
func WithSeedAddrs(addrs ...string) Option {
	return func(o *Options) {
		o.SeedAddrs = addrs
	}
}

// WithTTL sets the default TTL.
func WithTTL(ttl time.Duration) Option {
	return func(o *Options) {
		o.TTL = ttl
	}
}

// WithHotReadCacheTTL sets the hot read cache TTL.
func WithHotReadCacheTTL(ttl time.Duration) Option {
	return func(o *Options) {
		o.HotReadCacheTTL = ttl
	}
}

// WithHotCacheSize sets the hot read cache size.
func WithHotCacheSize(size int) Option {
	return func(o *Options) {
		o.HotCacheSize = size
	}
}

// WithVirtualNodes sets the number of virtual nodes.
func WithVirtualNodes(count int) Option {
	return func(o *Options) {
		o.VirtualNodes = count
	}
}

// WithReplicaCount sets the replica count.
func WithReplicaCount(count int) Option {
	return func(o *Options) {
		o.ReplicaCount = count
	}
}

// WithReadRepairRateLimit sets the read repair rate limit per second.
func WithReadRepairRateLimit(limit int64) Option {
	return func(o *Options) {
		o.ReadRepairRateLimitPerSec = limit
	}
}

// WithFailureTimeout sets the failure timeout.
func WithFailureTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.FailureTimeout = timeout
	}
}

// WithSuspectTimeout sets the suspect timeout.
func WithSuspectTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.SuspectTimeout = timeout
	}
}

// WithGossipInterval sets the gossip interval.
func WithGossipInterval(interval time.Duration) Option {
	return func(o *Options) {
		o.GossipInterval = interval
	}
}

// WithReplicationTimeout sets the replication timeout.
func WithReplicationTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.ReplicationTimeout = timeout
	}
}

// WithReadTimeout sets the read timeout.
func WithReadTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.ReadTimeout = timeout
	}
}

// WithStartupGracePeriod sets the startup grace period.
func WithStartupGracePeriod(period time.Duration) Option {
	return func(o *Options) {
		o.StartupGracePeriod = period
	}
}

// WithDisableAuth disables authentication.
func WithDisableAuth(disable bool) Option {
	return func(o *Options) {
		o.DisableAuth = disable
	}
}

// WithBatchThreshold sets the batch threshold.
func WithBatchThreshold(threshold int) Option {
	return func(o *Options) {
		o.BatchThreshold = threshold
	}
}

// WithBatchWindow sets the batch window.
func WithBatchWindow(window time.Duration) Option {
	return func(o *Options) {
		o.BatchWindow = window
	}
}

// WithNetworkType sets the network type.
func WithNetworkType(netType NetworkType) Option {
	return func(o *Options) {
		if o.Network == nil {
			o.Network = &NetworkOptions{}
		}
		o.Network.Type = netType
	}
}

// WithNetworkBindAddr sets the network bind address.
func WithNetworkBindAddr(addr string) Option {
	return func(o *Options) {
		if o.Network == nil {
			o.Network = &NetworkOptions{}
		}
		o.Network.BindAddr = addr
	}
}

// WithNetworkMaxIdle sets the maximum idle connections per peer.
func WithNetworkMaxIdle(maxIdle int) Option {
	return func(o *Options) {
		if o.Network == nil {
			o.Network = &NetworkOptions{}
		}
		o.Network.MaxIdle = maxIdle
	}
}

// WithNetworkMaxConns sets the maximum total connections per peer.
func WithNetworkMaxConns(maxConns int) Option {
	return func(o *Options) {
		if o.Network == nil {
			o.Network = &NetworkOptions{}
		}
		o.Network.MaxConns = maxConns
	}
}

// WithNetworkTimeout sets the network dial timeout.
func WithNetworkTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		if o.Network == nil {
			o.Network = &NetworkOptions{}
		}
		o.Network.Timeout = timeout
	}
}

// WithNetworkReadTimeout sets the network read timeout.
func WithNetworkReadTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		if o.Network == nil {
			o.Network = &NetworkOptions{}
		}
		o.Network.ReadTimeout = timeout
	}
}

// WithNetworkWriteTimeout sets the network write timeout.
func WithNetworkWriteTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		if o.Network == nil {
			o.Network = &NetworkOptions{}
		}
		o.Network.WriteTimeout = timeout
	}
}

// WithStorageMaxMemoryMB sets the maximum memory in MB.
func WithStorageMaxMemoryMB(mb int64) Option {
	return func(o *Options) {
		if o.Storage == nil {
			o.Storage = &StorageOptions{}
		}
		o.Storage.MaxMemoryMB = mb
	}
}

// WithStorageShardCount sets the shard count.
func WithStorageShardCount(count int) Option {
	return func(o *Options) {
		if o.Storage == nil {
			o.Storage = &StorageOptions{}
		}
		o.Storage.ShardCount = count
	}
}

// WithLogger sets the logger.
func WithLogger(log interface{}) Option {
	return func(o *Options) {
		o.Log = log
	}
}

// WithLoggerOptions sets the logger options.
func WithLoggerOptions(opts LoggerOptions) Option {
	return func(o *Options) {
		o.Log = opts
	}
}
