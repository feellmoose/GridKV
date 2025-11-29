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

	// Stage 2.1: Scale gossip/SWIM parameters by cluster size
	// Base values are set by profile preset above, then adjusted by cluster size
	// This replaces the old hardcoded defaults and prevents linear scaling
	p.adjustGossipParamsByClusterSize()

	// Network profile-based timeout configuration (Stage 1.3)
	// LAN: 0.5-0.8s, WAN: 1-2s, Global: 2-3s
	var readTimeout, replicationTimeout time.Duration
	switch p.Preset {
	case ProfileEdge:
		// LAN profile: tight timeouts for low latency
		readTimeout = defaultDuration(p.ReadTimeout, 600*time.Millisecond)
		if readTimeout < 500*time.Millisecond {
			readTimeout = 500 * time.Millisecond
		}
		if readTimeout > 800*time.Millisecond {
			readTimeout = 800 * time.Millisecond
		}
		replicationTimeout = defaultDuration(p.ReplicationTimeout, readTimeout)
		if replicationTimeout < 500*time.Millisecond {
			replicationTimeout = 500 * time.Millisecond
		}
		if replicationTimeout > 800*time.Millisecond {
			replicationTimeout = 800 * time.Millisecond
		}
	case ProfileGlobal:
		// Global profile: relaxed timeouts for cross-region
		readTimeout = defaultDuration(p.ReadTimeout, 2*time.Second)
		if readTimeout < 2*time.Second {
			readTimeout = 2 * time.Second
		}
		if readTimeout > 3*time.Second {
			readTimeout = 3 * time.Second
		}
		replicationTimeout = defaultDuration(p.ReplicationTimeout, readTimeout)
		if replicationTimeout < 2*time.Second {
			replicationTimeout = 2 * time.Second
		}
		if replicationTimeout > 3*time.Second {
			replicationTimeout = 3 * time.Second
		}
	default: // ProfileRegional (WAN)
		// WAN profile: moderate timeouts
		readTimeout = defaultDuration(p.ReadTimeout, 1*time.Second)
		if readTimeout < 1*time.Second {
			readTimeout = 1 * time.Second
		}
		if readTimeout > 2*time.Second {
			readTimeout = 2 * time.Second
		}
		replicationTimeout = defaultDuration(p.ReplicationTimeout, readTimeout)
		if replicationTimeout < 1*time.Second {
			replicationTimeout = 1 * time.Second
		}
		if replicationTimeout > 2*time.Second {
			replicationTimeout = 2 * time.Second
		}
	}
	p.ReadTimeout = readTimeout
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

// adjustGossipParamsByClusterSize adjusts gossip/SWIM parameters based on cluster size (Stage 2.1).
// This prevents parameters from scaling linearly with cluster size.
// Profile preset already sets base values, this function adjusts them by cluster size tier.
func (p *ClusterProfile) adjustGossipParamsByClusterSize() {
	size := p.ClusterSizeHint
	if size <= 0 {
		size = 3
	}

	// Get current values (may be set by profile preset or user)
	baseGossipInterval := p.GossipInterval
	baseFailureTimeout := p.FailureTimeout
	baseSuspectTimeout := p.SuspectTimeout

	// Stage 2.1: Adjust by cluster size tier while respecting profile base values
	// Small (≤10): tight intervals for fast convergence
	// Medium (10-50): moderate intervals
	// Large (>50): relaxed intervals to reduce overhead
	switch {
	case size <= 10:
		// Small cluster: 200-400ms gossip, 1.5-2s failure, 3-5s suspect
		if baseGossipInterval == 0 {
			baseGossipInterval = 300 * time.Millisecond
		}
		baseGossipInterval = clampDuration(baseGossipInterval, 200*time.Millisecond, 400*time.Millisecond)
		
		if baseFailureTimeout == 0 {
			baseFailureTimeout = baseGossipInterval * 5
		}
		baseFailureTimeout = clampDuration(baseFailureTimeout, 1500*time.Millisecond, 2*time.Second)
		
		if baseSuspectTimeout == 0 {
			baseSuspectTimeout = baseFailureTimeout * 2
		}
		baseSuspectTimeout = clampDuration(baseSuspectTimeout, 3*time.Second, 5*time.Second)

	case size <= 50:
		// Medium cluster: 400-750ms gossip, 2-2.5s failure, 5-8s suspect
		if baseGossipInterval == 0 {
			baseGossipInterval = 500 * time.Millisecond
		}
		baseGossipInterval = clampDuration(baseGossipInterval, 400*time.Millisecond, 750*time.Millisecond)
		
		if baseFailureTimeout == 0 {
			baseFailureTimeout = baseGossipInterval * 5
		}
		baseFailureTimeout = clampDuration(baseFailureTimeout, 2*time.Second, 2500*time.Millisecond)
		
		if baseSuspectTimeout == 0 {
			baseSuspectTimeout = baseFailureTimeout * 2
		}
		baseSuspectTimeout = clampDuration(baseSuspectTimeout, 5*time.Second, 8*time.Second)

	default:
		// Large cluster (>50): 750-1000ms gossip, 2.5-3s failure, 8-10s suspect
		if baseGossipInterval == 0 {
			baseGossipInterval = 850 * time.Millisecond
		}
		baseGossipInterval = clampDuration(baseGossipInterval, 750*time.Millisecond, 1000*time.Millisecond)
		
		if baseFailureTimeout == 0 {
			baseFailureTimeout = baseGossipInterval * 5
		}
		baseFailureTimeout = clampDuration(baseFailureTimeout, 2500*time.Millisecond, 3*time.Second)
		
		if baseSuspectTimeout == 0 {
			baseSuspectTimeout = baseFailureTimeout * 2
		}
		baseSuspectTimeout = clampDuration(baseSuspectTimeout, 8*time.Second, 10*time.Second)
	}

	p.GossipInterval = baseGossipInterval
	p.FailureTimeout = baseFailureTimeout
	p.SuspectTimeout = baseSuspectTimeout
}

func clampDuration(value, minVal, maxVal time.Duration) time.Duration {
	if value < minVal {
		return minVal
	}
	if value > maxVal {
		return maxVal
	}
	return value
}
