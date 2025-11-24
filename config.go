package gridkv

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/feellmoose/gridkv/internal/metrics"
	utilcluster "github.com/feellmoose/gridkv/internal/utils/cluster"
	"github.com/feellmoose/gridkv/internal/utils/crypto"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// NetworkType represents the network transport protocol.
type NetworkType int

const (
	TCP  NetworkType = 1 // Reliable transport (compatibility)
	QUIC NetworkType = 3 // High-performance QUIC (large clusters)
	UDP  NetworkType = 4
)

// NetworkOptions configures the network transport layer. All fields optional (defaults from profile).
type NetworkOptions struct {
	Type         NetworkType   // Transport protocol (0 = profile default, default QUIC)
	BindAddr     string        // Local bind address (default: LocalAddress)
	MaxIdle      int           // Max idle connections per peer
	MaxConns     int           // Max total connections per peer
	Timeout      time.Duration // Connection timeout
	ReadTimeout  time.Duration // Read operation timeout
	WriteTimeout time.Duration // Write operation timeout
}

// StorageBackendType identifies the storage backend implementation.
type StorageBackendType string

const (
	BackendMemory        StorageBackendType = "Memory"        // sync.Map backend (600-700K ops/s)
	BackendMemorySharded StorageBackendType = "MemorySharded" // Sharded backend (1-2M+ ops/s, production)
)

// StorageOptions configures the storage backend. All fields optional (defaults from profile).
type StorageOptions struct {
	Backend     StorageBackendType // Storage implementation ("" = profile default, default MemorySharded)
	MaxMemoryMB int64              // Memory limit in MB (0 = profile default, default 4096)
	ShardCount  int                // Shards for MemorySharded (0 = profile default, default 256)
}

// ClusterProfile exports automatic configuration profile types.
type (
	ClusterProfile  = utilcluster.ClusterProfile
	ProfilePreset   = utilcluster.ProfilePreset
	TransportPreset = utilcluster.TransportPreset
	NetworkProfile  = utilcluster.NetworkProfile
	StorageProfile  = utilcluster.StorageProfile
)

const (
	ProfileEdge     = utilcluster.ProfileEdge
	ProfileRegional = utilcluster.ProfileRegional
	ProfileGlobal   = utilcluster.ProfileGlobal

	ClusterTransportTCP  = utilcluster.TransportTCP
	ClusterTransportQUIC = utilcluster.TransportQUIC
	ClusterTransportUDP  = utilcluster.TransportUDP
)

// DefaultClusterProfile returns the recommended profile for a cluster size.
func DefaultClusterProfile(clusterSizeHint int) *ClusterProfile {
	return utilcluster.DefaultClusterProfile(clusterSizeHint)
}

// NewClusterProfile builds a custom profile.
func NewClusterProfile(preset ProfilePreset, transport TransportPreset, clusterSizeHint int) *ClusterProfile {
	return utilcluster.NewClusterProfile(preset, transport, clusterSizeHint)
}

// GridKVOptions configures a GridKV instance.
type GridKVOptions struct {
	// Required
	LocalNodeID  string
	LocalAddress string
	SeedAddrs    []string

	// Configuration
	Profile *ClusterProfile
	Network *NetworkOptions
	Storage *StorageOptions
	Log     *logging.LogOptions

	// Replication
	ReplicaCount       int
	VirtualNodes       int
	MaxReplicators     int
	ReplicationTimeout time.Duration
	ReadTimeout        time.Duration

	// Failure detection
	FailureTimeout     time.Duration
	SuspectTimeout     time.Duration
	GossipInterval     time.Duration
	StartupGracePeriod time.Duration

	// Data
	DataCenter string
	TTL        time.Duration

	// Security
	KeyPair        *crypto.KeyPair
	PeerPublicKeys map[string]crypto.PublicKey
	DisableAuth    bool

	// Advanced tuning
	MigrateRateLimitPerSec    int64
	ReadRepairRateLimitPerSec int64
	HotReadCacheTTL           time.Duration

	// Metrics
	Metrics *metrics.GridKVMetrics
}

// applyDefaults applies default values to GridKVOptions based on profile.
func applyDefaults(opts *GridKVOptions) {
	clusterSizeHint := len(opts.SeedAddrs) + 1
	if clusterSizeHint == 0 {
		clusterSizeHint = 1
	}

	// Initialize or clone profile
	profile := opts.Profile
	if profile == nil {
		profile = DefaultClusterProfile(clusterSizeHint)
	} else {
		profile = profile.Clone()
		if profile.ClusterSizeHint == 0 {
			profile.ClusterSizeHint = clusterSizeHint
		}
		profile.ApplyDefaults()
	}
	opts.Profile = profile

	// Apply profile defaults to top-level options
	if opts.ReplicaCount <= 0 {
		opts.ReplicaCount = profile.ReplicaCount
	}
	if opts.VirtualNodes <= 0 {
		opts.VirtualNodes = profile.VirtualNodes
	}
	if opts.MaxReplicators <= 0 {
		opts.MaxReplicators = profile.MaxReplicators
	}
	if opts.ReplicationTimeout == 0 {
		opts.ReplicationTimeout = profile.ReplicationTimeout
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = profile.ReadTimeout
	}
	if opts.FailureTimeout == 0 {
		opts.FailureTimeout = profile.FailureTimeout
	}
	if opts.SuspectTimeout == 0 {
		opts.SuspectTimeout = profile.SuspectTimeout
	}
	if opts.GossipInterval == 0 {
		opts.GossipInterval = profile.GossipInterval
	}
	if opts.StartupGracePeriod == 0 {
		opts.StartupGracePeriod = profile.StartupGracePeriod
	}
	if opts.MigrateRateLimitPerSec == 0 {
		opts.MigrateRateLimitPerSec = profile.MigrateRateLimitPerSec
	}
	if opts.ReadRepairRateLimitPerSec == 0 {
		opts.ReadRepairRateLimitPerSec = profile.ReadRepairRateLimitPerSec
	}
	if opts.HotReadCacheTTL == 0 {
		opts.HotReadCacheTTL = profile.HotReadCacheTTL
	}
	opts.DisableAuth = opts.DisableAuth || profile.DisableAuth

	// Default VirtualNodes if still not set
	if opts.VirtualNodes <= 0 {
		opts.VirtualNodes = 150
	}

	// Derive network options
	opts.Network = deriveNetworkOptions(opts.Network, profile, opts.LocalAddress)

	// Derive storage options
	opts.Storage = deriveStorageOptions(opts.Storage, profile)
}

// deriveNetworkOptions creates NetworkOptions with profile defaults.
func deriveNetworkOptions(user *NetworkOptions, profile *ClusterProfile, bindAddr string) *NetworkOptions {
	var derived NetworkOptions
	if user != nil {
		derived = *user
	}

	// Set defaults from profile
	if derived.Type == 0 {
		derived.Type = networkTypeFromPreset(profile.Transport)
	}
	if derived.BindAddr == "" {
		derived.BindAddr = bindAddr
	}
	if derived.MaxIdle == 0 {
		derived.MaxIdle = profile.Network.MaxIdle
	}
	if derived.MaxConns == 0 {
		derived.MaxConns = profile.Network.MaxConns
	}
	if derived.Timeout == 0 {
		derived.Timeout = profile.Network.Timeout
	}
	if derived.ReadTimeout == 0 {
		derived.ReadTimeout = profile.Network.ReadTimeout
	}
	if derived.WriteTimeout == 0 {
		derived.WriteTimeout = profile.Network.WriteTimeout
	}

	return &derived
}

// deriveStorageOptions creates StorageOptions with profile defaults.
func deriveStorageOptions(user *StorageOptions, profile *ClusterProfile) *StorageOptions {
	var derived StorageOptions
	if user != nil {
		derived = *user
	}

	// Set defaults from profile
	if derived.Backend == "" {
		derived.Backend = StorageBackendType(profile.Storage.Backend)
		if derived.Backend == "" {
			derived.Backend = BackendMemorySharded
		}
	}
	if derived.MaxMemoryMB == 0 {
		derived.MaxMemoryMB = profile.Storage.MaxMemoryMB
		if derived.MaxMemoryMB == 0 {
			derived.MaxMemoryMB = 4096
		}
	}
	if derived.ShardCount == 0 {
		derived.ShardCount = profile.Storage.ShardCount
		if derived.ShardCount == 0 {
			if derived.Backend == BackendMemorySharded {
				derived.ShardCount = 256
			} else {
				derived.ShardCount = 1
			}
		}
	}

	return &derived
}

func networkTypeFromPreset(preset TransportPreset) NetworkType {
	switch preset {
	case ClusterTransportTCP:
		return TCP
	case ClusterTransportUDP:
		return UDP
	default:
		return QUIC
	}
}

// configFile represents JSON configuration file structure.
type configFile struct {
	LocalNodeID  string   `json:"local_node_id"`
	LocalAddress string   `json:"local_address"`
	SeedAddrs    []string `json:"seed_addrs,omitempty"`

	Profile *profileConfig `json:"profile,omitempty"`

	Network *networkConfig `json:"network,omitempty"`
	Storage *storageConfig `json:"storage,omitempty"`

	Log *logConfig `json:"log,omitempty"`

	ReplicaCount        int    `json:"replica_count,omitempty"`
	VirtualNodes        int    `json:"virtual_nodes,omitempty"`
	MaxReplicators      int    `json:"max_replicators,omitempty"`
	ReplicationTimeout  string `json:"replication_timeout,omitempty"`
	ReadTimeout         string `json:"read_timeout,omitempty"`
	FailureTimeout      string `json:"failure_timeout,omitempty"`
	SuspectTimeout      string `json:"suspect_timeout,omitempty"`
	GossipInterval      string `json:"gossip_interval,omitempty"`
	StartupGracePeriod  string `json:"startup_grace_period,omitempty"`
	DataCenter          string `json:"data_center,omitempty"`
	TTL                 string `json:"ttl,omitempty"`
	DisableAuth         bool   `json:"disable_auth,omitempty"`
	MigrateRateLimit    int64  `json:"migrate_rate_limit_per_sec,omitempty"`
	ReadRepairRateLimit int64  `json:"read_repair_rate_limit_per_sec,omitempty"`
	HotReadCacheTTL     string `json:"hot_read_cache_ttl,omitempty"`
}

type profileConfig struct {
	Preset          string `json:"preset,omitempty"`
	Transport       string `json:"transport,omitempty"`
	ClusterSizeHint int    `json:"cluster_size_hint,omitempty"`
}

type networkConfig struct {
	Type         string `json:"type,omitempty"`
	BindAddr     string `json:"bind_addr,omitempty"`
	MaxIdle      int    `json:"max_idle,omitempty"`
	MaxConns     int    `json:"max_conns,omitempty"`
	Timeout      string `json:"timeout,omitempty"`
	ReadTimeout  string `json:"read_timeout,omitempty"`
	WriteTimeout string `json:"write_timeout,omitempty"`
}

type storageConfig struct {
	Backend     string `json:"backend,omitempty"`
	MaxMemoryMB int64  `json:"max_memory_mb,omitempty"`
	ShardCount  int    `json:"shard_count,omitempty"`
}

type logConfig struct {
	Level       string `json:"level,omitempty"`
	Format      string `json:"format,omitempty"`
	EnableDebug bool   `json:"enable_debug,omitempty"`
}

// LoadConfigFromFile loads GridKVOptions from a JSON file.
func LoadConfigFromFile(path string) (*GridKVOptions, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg configFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	opts := &GridKVOptions{
		LocalNodeID:  cfg.LocalNodeID,
		LocalAddress: cfg.LocalAddress,
		SeedAddrs:    cfg.SeedAddrs,
		DataCenter:   cfg.DataCenter,
		DisableAuth:  cfg.DisableAuth,
	}

	if cfg.Profile != nil {
		preset := utilcluster.ProfilePreset(cfg.Profile.Preset)
		transport := utilcluster.TransportPreset(cfg.Profile.Transport)
		opts.Profile = utilcluster.NewClusterProfile(preset, transport, cfg.Profile.ClusterSizeHint)
	}

	if cfg.Network != nil {
		networkType := TCP
		switch cfg.Network.Type {
		case "tcp", "TCP":
			networkType = TCP
		case "quic", "QUIC":
			networkType = QUIC
		case "udp", "UDP":
			networkType = UDP
		}

		opts.Network = &NetworkOptions{
			Type:     networkType,
			BindAddr: cfg.Network.BindAddr,
			MaxIdle:  cfg.Network.MaxIdle,
			MaxConns: cfg.Network.MaxConns,
		}

		if cfg.Network.Timeout != "" {
			if d, err := parseDuration(cfg.Network.Timeout); err == nil {
				opts.Network.Timeout = d
			}
		}
		if cfg.Network.ReadTimeout != "" {
			if d, err := parseDuration(cfg.Network.ReadTimeout); err == nil {
				opts.Network.ReadTimeout = d
			}
		}
		if cfg.Network.WriteTimeout != "" {
			if d, err := parseDuration(cfg.Network.WriteTimeout); err == nil {
				opts.Network.WriteTimeout = d
			}
		}
	}

	if cfg.Storage != nil {
		opts.Storage = &StorageOptions{
			Backend:     StorageBackendType(cfg.Storage.Backend),
			MaxMemoryMB: cfg.Storage.MaxMemoryMB,
			ShardCount:  cfg.Storage.ShardCount,
		}
	}

	if cfg.Log != nil {
		opts.Log = &logging.LogOptions{
			Level:       cfg.Log.Level,
			Format:      cfg.Log.Format,
			EnableDebug: cfg.Log.EnableDebug,
		}
	}

	if cfg.ReplicaCount > 0 {
		opts.ReplicaCount = cfg.ReplicaCount
	}
	if cfg.VirtualNodes > 0 {
		opts.VirtualNodes = cfg.VirtualNodes
	}
	if cfg.MaxReplicators > 0 {
		opts.MaxReplicators = cfg.MaxReplicators
	}
	if cfg.MigrateRateLimit > 0 {
		opts.MigrateRateLimitPerSec = cfg.MigrateRateLimit
	}
	if cfg.ReadRepairRateLimit > 0 {
		opts.ReadRepairRateLimitPerSec = cfg.ReadRepairRateLimit
	}

	if cfg.ReplicationTimeout != "" {
		if d, err := parseDuration(cfg.ReplicationTimeout); err == nil {
			opts.ReplicationTimeout = d
		}
	}
	if cfg.ReadTimeout != "" {
		if d, err := parseDuration(cfg.ReadTimeout); err == nil {
			opts.ReadTimeout = d
		}
	}
	if cfg.FailureTimeout != "" {
		if d, err := parseDuration(cfg.FailureTimeout); err == nil {
			opts.FailureTimeout = d
		}
	}
	if cfg.SuspectTimeout != "" {
		if d, err := parseDuration(cfg.SuspectTimeout); err == nil {
			opts.SuspectTimeout = d
		}
	}
	if cfg.GossipInterval != "" {
		if d, err := parseDuration(cfg.GossipInterval); err == nil {
			opts.GossipInterval = d
		}
	}
	if cfg.StartupGracePeriod != "" {
		if d, err := parseDuration(cfg.StartupGracePeriod); err == nil {
			opts.StartupGracePeriod = d
		}
	}
	if cfg.TTL != "" {
		if d, err := parseDuration(cfg.TTL); err == nil {
			opts.TTL = d
		}
	}
	if cfg.HotReadCacheTTL != "" {
		if d, err := parseDuration(cfg.HotReadCacheTTL); err == nil {
			opts.HotReadCacheTTL = d
		}
	}

	return opts, nil
}

// parseDuration parses duration strings (e.g., "500ms", "2s", "1h30m").
func parseDuration(s string) (time.Duration, error) {
	return time.ParseDuration(s)
}
