package cluster

import (
	"github.com/feellmoose/gridkv/internal/mem_storage"
)

// Message types for cluster communication
// All types that need serialization are collected here

// SyncOperation represents a sync operation for gossip/replay
// Reuses mem_storage.SyncOperation to avoid duplication
type SyncOperation = mem_storage.SyncOperation

// Member messages for SWIM protocol
type pingMsg struct {
	From        string
	To          string
	Incarnation int64
}

type connectMsg struct {
	NodeID      string
	Address     string
	Incarnation int64
}

type leaveMsg struct {
	NodeID      string
	Incarnation int64
}

type ackMsg struct {
	From        string
	To          string
	Incarnation int64
}

type indirectProbeMsg struct {
	From        string
	Target      string
	Incarnation int64
}

type indirectAckMsg struct {
	From        string
	To          string
	Target      string
	Incarnation int64
}

type clusterSyncMsg struct {
	From    string
	Members []NodeInfo
}
