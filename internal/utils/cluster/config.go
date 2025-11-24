package cluster

import "time"

// ProfilePreset identifies the intended deployment tier.
type ProfilePreset string

const (
	// ProfileEdge favors ultra-low latency and tiny clusters (≤5 nodes).
	ProfileEdge ProfilePreset = "edge"
	// ProfileRegional is optimized for typical 5-50 node regional clusters.
	ProfileRegional ProfilePreset = "regional"
	// ProfileGlobal focuses on cross-region or >50 node clusters.
	ProfileGlobal ProfilePreset = "global"
)

// TransportPreset selects the transport strategy for the cluster.
type TransportPreset string

const (
	// TransportTCP uses the default TCP transport.
	TransportTCP TransportPreset = "tcp"
	// TransportQUIC enables QUIC with UDP fallback (recommended).
	TransportQUIC TransportPreset = "quic"
	TransportUDP  TransportPreset = "udp"
)

// ClusterProfile encapsulates all derived tuning knobs for a deployment.
type ClusterProfile struct {
	Preset          ProfilePreset
	Transport       TransportPreset
	ClusterSizeHint int

	ReplicaCount   int
	VirtualNodes   int
	MaxReplicators int

	GossipInterval     time.Duration
	FailureTimeout     time.Duration
	SuspectTimeout     time.Duration
	ReplicationTimeout time.Duration
	ReadTimeout        time.Duration
	StartupGracePeriod time.Duration

	HotReadCacheTTL time.Duration

	Network NetworkProfile
	Storage StorageProfile

	MigrateRateLimitPerSec    int64
	ReadRepairRateLimitPerSec int64
	DisableAuth               bool
}

// NetworkProfile captures default connection pool sizing and timeouts.
type NetworkProfile struct {
	MaxIdle      int
	MaxConns     int
	Timeout      time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// StorageProfile describes the recommended in-memory backend parameters.
type StorageProfile struct {
	Backend     string
	MaxMemoryMB int64
	ShardCount  int
}

// DefaultClusterProfile returns a tuned profile using the regional preset.
func DefaultClusterProfile(clusterSizeHint int) *ClusterProfile {
	return NewClusterProfile(ProfileRegional, TransportQUIC, clusterSizeHint)
}

// NewClusterProfile builds a profile for the requested preset/transport.
func NewClusterProfile(preset ProfilePreset, transport TransportPreset, clusterSizeHint int) *ClusterProfile {
	p := &ClusterProfile{
		Preset:          preset,
		Transport:       transport,
		ClusterSizeHint: clusterSizeHint,
	}
	p.applyDefaults()
	return p
}

// Clone returns a deep copy that can be safely mutated by callers.
func (p *ClusterProfile) Clone() *ClusterProfile {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// ApplyDefaults normalizes the profile in-place.
func (p *ClusterProfile) ApplyDefaults() {
	p.applyDefaults()
}

func (p *ClusterProfile) applyDefaults() {
	if p.Preset == "" {
		p.Preset = ProfileRegional
	}
	if p.Transport == "" {
		p.Transport = TransportQUIC
	}
	if p.ClusterSizeHint <= 0 {
		p.ClusterSizeHint = 3
	}

	switch p.Preset {
	case ProfileEdge:
		p.ReplicaCount = defaultInt(p.ReplicaCount, minInt(2, p.ClusterSizeHint))
		p.GossipInterval = defaultDuration(p.GossipInterval, 200*time.Millisecond)
		p.StartupGracePeriod = defaultDuration(p.StartupGracePeriod, 5*time.Second)
		p.HotReadCacheTTL = defaultDuration(p.HotReadCacheTTL, 5*time.Millisecond)
	case ProfileGlobal:
		p.ReplicaCount = defaultInt(p.ReplicaCount, clampInt(p.ClusterSizeHint/10, 3, 5))
		p.GossipInterval = defaultDuration(p.GossipInterval, 750*time.Millisecond)
		p.StartupGracePeriod = defaultDuration(p.StartupGracePeriod, 45*time.Second)
		p.HotReadCacheTTL = defaultDuration(p.HotReadCacheTTL, 50*time.Millisecond)
	default: // ProfileRegional
		p.ReplicaCount = defaultInt(p.ReplicaCount, clampInt(p.ClusterSizeHint/5, 3, 4))
		p.GossipInterval = defaultDuration(p.GossipInterval, 400*time.Millisecond)
		p.StartupGracePeriod = defaultDuration(p.StartupGracePeriod, 15*time.Second)
		p.HotReadCacheTTL = defaultDuration(p.HotReadCacheTTL, 15*time.Millisecond)
	}

	if p.ReplicaCount < 2 {
		p.ReplicaCount = 2
	}
	if p.ReplicaCount > 5 {
		p.ReplicaCount = 5
	}

	if p.VirtualNodes <= 0 {
		switch {
		case p.ClusterSizeHint <= 8:
			p.VirtualNodes = 128
		case p.ClusterSizeHint <= 64:
			p.VirtualNodes = 256
		default:
			p.VirtualNodes = 512
		}
	}

	if p.MaxReplicators <= 0 {
		p.MaxReplicators = clampInt(p.ReplicaCount*2, 4, 16)
	}

	baseFailure := defaultDuration(p.FailureTimeout, p.GossipInterval*5)
	if baseFailure < 5*time.Second {
		baseFailure = 5 * time.Second
	}
	p.FailureTimeout = baseFailure

	baseSuspect := defaultDuration(p.SuspectTimeout, p.FailureTimeout*2)
	if baseSuspect < 10*time.Second {
		baseSuspect = 10 * time.Second
	}
	p.SuspectTimeout = baseSuspect

	// Increased default read timeout for better reliability under high load
	readTimeout := defaultDuration(p.ReadTimeout, maxDuration(5*time.Second, p.GossipInterval*8))
	if readTimeout > 15*time.Second {
		readTimeout = 15 * time.Second // Increased max for high load scenarios
	}
	// Ensure minimum timeout for stability
	if readTimeout < 3*time.Second {
		readTimeout = 3 * time.Second
	}
	p.ReadTimeout = readTimeout

	replicationTimeout := defaultDuration(p.ReplicationTimeout, p.ReadTimeout)
	p.ReplicationTimeout = replicationTimeout

	if p.Network.MaxIdle == 0 {
		p.Network.MaxIdle = clampInt(p.ClusterSizeHint*4, 64, 512)
	}
	if p.Network.MaxConns == 0 {
		p.Network.MaxConns = clampInt(p.ClusterSizeHint*16, 256, 4096)
	}
	if p.Network.Timeout == 0 {
		p.Network.Timeout = 30 * time.Second
	}
	if p.Network.ReadTimeout == 0 {
		p.Network.ReadTimeout = p.ReadTimeout
	}
	if p.Network.WriteTimeout == 0 {
		p.Network.WriteTimeout = p.ReadTimeout
	}

	if p.Storage.Backend == "" {
		// Default to MemorySharded for high performance
		p.Storage.Backend = "MemorySharded"
	}
	if p.Storage.ShardCount == 0 {
		if p.Storage.Backend == "Memory" {
			p.Storage.ShardCount = 1
		} else if p.ClusterSizeHint > 64 {
			p.Storage.ShardCount = 512
		} else {
			p.Storage.ShardCount = 256
		}
	}
	if p.Storage.MaxMemoryMB == 0 {
		if p.ClusterSizeHint <= 3 {
			p.Storage.MaxMemoryMB = 512
		} else {
			p.Storage.MaxMemoryMB = 4096
		}
	}

	if p.MigrateRateLimitPerSec == 0 {
		p.MigrateRateLimitPerSec = int64(clampInt(p.ClusterSizeHint*2000, 20000, 200000))
	}
	if p.ReadRepairRateLimitPerSec == 0 {
		p.ReadRepairRateLimitPerSec = int64(clampInt(p.ClusterSizeHint*2000, 20000, 150000))
	}
}

func defaultInt(value, fallback int) int {
	if value != 0 {
		return value
	}
	return fallback
}

func clampInt(value, minVal, maxVal int) int {
	if value < minVal {
		return minVal
	}
	if value > maxVal {
		return maxVal
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func defaultDuration(value, fallback time.Duration) time.Duration {
	if value != 0 {
		return value
	}
	return fallback
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
