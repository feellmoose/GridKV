package gossip

import "time"

// NodeState defines the health status of a node.
type NodeState int32

const (
	NodeStateUnknown NodeState = 0
	NodeStateAlive   NodeState = 1
	NodeStateSuspect NodeState = 2
	NodeStateDead    NodeState = 3
)

// GossipMessageType defines the type of message being transmitted.
type GossipMessageType int32

const (
	MessageTypeUnknown           GossipMessageType = 0
	MessageTypeCacheSync         GossipMessageType = 1
	MessageTypeClusterSync       GossipMessageType = 2
	MessageTypeConnect           GossipMessageType = 3
	MessageTypeProbeRequest      GossipMessageType = 4
	MessageTypeProbeResponse     GossipMessageType = 5
	MessageTypeCacheSyncAck      GossipMessageType = 6
	MessageTypeFullSyncRequest   GossipMessageType = 7
	MessageTypeFullSyncResponse  GossipMessageType = 8
	MessageTypeReadRequest       GossipMessageType = 9
	MessageTypeReadResponse      GossipMessageType = 10
	MessageTypeBatchReadRequest  GossipMessageType = 11
	MessageTypeBatchReadResponse GossipMessageType = 12
)

// OperationType specifies the explicit action being performed on a cache item.
type OperationType int32

const (
	OpUnspecified OperationType = 0
	OpSet         OperationType = 1
	OpDelete      OperationType = 2
)

// NodeInfo represents metadata for a single cluster member.
type NodeInfo struct {
	NodeId       string
	Address      string
	LastActiveTs time.Time
	State        NodeState
	Version      int64
}

// ConnectPayload is sent when a new node joins the cluster.
type ConnectPayload struct {
	NodeId    string
	Address   string
	Version   int64
	Hlc       string
	PublicKey []byte
}

// ClusterSyncPayload contains a list of known nodes for state synchronization.
type ClusterSyncPayload struct {
	Nodes []*NodeInfo
}

// ProbePayload is used for indirect failure detection.
type ProbePayload struct {
	TargetNodeId string
	RequesterId  string
}

// ProbeResponsePayload is the response to an indirect probe.
type ProbeResponsePayload struct {
	TargetNodeId string
	Alive        bool
}

// StoredItem contains the actual data being stored for SET/UPDATE operations.
type StoredItem struct {
	ExpireAt uint64
	Value    []byte
}

// CacheSyncOperation defines a single, atomic change to a cache item.
type CacheSyncOperation struct {
	Key           string
	ClientVersion int64
	Type          OperationType
	SetData       *StoredItem
	DataPayload   interface{} // For compatibility with protobuf oneof
}

// GetSetData returns SetData if Type is OpSet
func (op *CacheSyncOperation) GetSetData() *StoredItem {
	if op.Type == OpSet {
		return op.SetData
	}
	return nil
}

// GetKey returns the key
func (op *CacheSyncOperation) GetKey() string {
	return op.Key
}

// IncrementalSyncPayload defines the structure for DELTA updates.
type IncrementalSyncPayload struct {
	Operations []*CacheSyncOperation
	PayloadId  string
}

// FullStateItem represents a complete key-value entry for the Full Sync mode.
type FullStateItem struct {
	Key      string
	Version  int64
	ItemData *StoredItem
}

// FullSyncPayload defines the structure for a COMPLETE state snapshot.
type FullSyncPayload struct {
	Items             []*FullStateItem
	SnapshotTimestamp uint64
}

// SyncMessage is the top-level wrapper for all CACHE synchronization data.
type SyncMessage struct {
	IncrementalSync *IncrementalSyncPayload
	FullSync        *FullSyncPayload
	SyncType        interface{} // For compatibility with protobuf oneof
}

// GetIncrementalSync returns IncrementalSync if present
func (sm *SyncMessage) GetIncrementalSync() *IncrementalSyncPayload {
	if sm.IncrementalSync != nil {
		return sm.IncrementalSync
	}
	if wrapper, ok := sm.SyncType.(*SyncMessage_IncrementalSync); ok {
		return wrapper.IncrementalSync
	}
	return nil
}

// GetFullSync returns FullSync if present
func (sm *SyncMessage) GetFullSync() *FullSyncPayload {
	if sm.FullSync != nil {
		return sm.FullSync
	}
	if wrapper, ok := sm.SyncType.(*SyncMessage_FullSync); ok {
		return wrapper.FullSync
	}
	return nil
}

// Compatibility wrappers
type SyncMessage_IncrementalSync struct {
	IncrementalSync *IncrementalSyncPayload
}

type SyncMessage_FullSync struct {
	FullSync *FullSyncPayload
}

// CacheSyncAckPayload is sent by a replica to acknowledge a replication OpId.
type CacheSyncAckPayload struct {
	OpId    string
	PeerId  string
	Success bool
}

// FullSyncRequestPayload is used to request a full state snapshot from a peer.
type FullSyncRequestPayload struct {
	RequesterId string
}

// FullSyncResponsePayload carries the complete snapshot as a response.
type FullSyncResponsePayload struct {
	FullSync *FullSyncPayload
}

// ReadRequestPayload is used to request a value for a specific key.
type ReadRequestPayload struct {
	Key         string
	RequesterId string
	RequestId   string
}

// ReadResponsePayload carries the value and metadata for a read request.
type ReadResponsePayload struct {
	Key         string
	RequestId   string
	Found       bool
	ItemData    *StoredItem
	Version     int64
	ResponderId string
}

// BatchReadRequestPayload contains multiple read requests for batching.
type BatchReadRequestPayload struct {
	Requests    []*ReadRequestPayload
	BatchId     string
	RequesterId string
}

// BatchReadResponsePayload contains multiple read responses.
type BatchReadResponsePayload struct {
	Responses   []*ReadResponsePayload
	BatchId     string
	ResponderId string
}

// GossipMessage encapsulates the payload for transmission.
type GossipMessage struct {
	Type       GossipMessageType
	Sender     string
	Signature  []byte
	OpId       string
	Hlc        string
	Compressed bool

	// Payload interface for type-safe access
	Payload isGossipMessage_Payload

	// Direct access fields (for convenience and performance)
	ConnectPayload           *ConnectPayload
	ClusterSyncPayload       *ClusterSyncPayload
	ProbeRequestPayload      *ProbePayload
	ProbeResponsePayload     *ProbeResponsePayload
	CacheSyncPayload         *SyncMessage
	CacheSyncAckPayload      *CacheSyncAckPayload
	FullSyncRequestPayload   *FullSyncRequestPayload
	FullSyncResponsePayload  *FullSyncResponsePayload
	ReadRequestPayload       *ReadRequestPayload
	ReadResponsePayload      *ReadResponsePayload
	BatchReadRequestPayload  *BatchReadRequestPayload
	BatchReadResponsePayload *BatchReadResponsePayload
}

// Compatibility wrapper types for protobuf-style access
type GossipMessage_ConnectPayload struct {
	ConnectPayload *ConnectPayload
}

type GossipMessage_ClusterSyncPayload struct {
	ClusterSyncPayload *ClusterSyncPayload
}

type GossipMessage_ProbeRequestPayload struct {
	ProbeRequestPayload *ProbePayload
}

type GossipMessage_ProbeResponsePayload struct {
	ProbeResponsePayload *ProbeResponsePayload
}

type GossipMessage_CacheSyncPayload struct {
	CacheSyncPayload *SyncMessage
}

type GossipMessage_CacheSyncAckPayload struct {
	CacheSyncAckPayload *CacheSyncAckPayload
}

type GossipMessage_FullSyncRequestPayload struct {
	FullSyncRequestPayload *FullSyncRequestPayload
}

type GossipMessage_FullSyncResponsePayload struct {
	FullSyncResponsePayload *FullSyncResponsePayload
}

type GossipMessage_ReadRequestPayload struct {
	ReadRequestPayload *ReadRequestPayload
}

type GossipMessage_ReadResponsePayload struct {
	ReadResponsePayload *ReadResponsePayload
}

type GossipMessage_BatchReadRequestPayload struct {
	BatchReadRequestPayload *BatchReadRequestPayload
}

type GossipMessage_BatchReadResponsePayload struct {
	BatchReadResponsePayload *BatchReadResponsePayload
}

// Payload interface for compatibility
type isGossipMessage_Payload interface {
	isGossipMessage_Payload()
}

func (*GossipMessage_ConnectPayload) isGossipMessage_Payload()           {}
func (*GossipMessage_ClusterSyncPayload) isGossipMessage_Payload()       {}
func (*GossipMessage_ProbeRequestPayload) isGossipMessage_Payload()      {}
func (*GossipMessage_ProbeResponsePayload) isGossipMessage_Payload()     {}
func (*GossipMessage_CacheSyncPayload) isGossipMessage_Payload()         {}
func (*GossipMessage_CacheSyncAckPayload) isGossipMessage_Payload()      {}
func (*GossipMessage_FullSyncRequestPayload) isGossipMessage_Payload()   {}
func (*GossipMessage_FullSyncResponsePayload) isGossipMessage_Payload()  {}
func (*GossipMessage_ReadRequestPayload) isGossipMessage_Payload()       {}
func (*GossipMessage_ReadResponsePayload) isGossipMessage_Payload()      {}
func (*GossipMessage_BatchReadRequestPayload) isGossipMessage_Payload()  {}
func (*GossipMessage_BatchReadResponsePayload) isGossipMessage_Payload() {}

// syncPayload syncs direct fields to Payload interface
func (msg *GossipMessage) syncPayload() {
	if msg.Payload != nil {
		return
	}
	switch msg.Type {
	case MessageTypeConnect:
		if msg.ConnectPayload != nil {
			msg.Payload = &GossipMessage_ConnectPayload{ConnectPayload: msg.ConnectPayload}
		}
	case MessageTypeClusterSync:
		if msg.ClusterSyncPayload != nil {
			msg.Payload = &GossipMessage_ClusterSyncPayload{ClusterSyncPayload: msg.ClusterSyncPayload}
		}
	case MessageTypeProbeRequest:
		if msg.ProbeRequestPayload != nil {
			msg.Payload = &GossipMessage_ProbeRequestPayload{ProbeRequestPayload: msg.ProbeRequestPayload}
		}
	case MessageTypeProbeResponse:
		if msg.ProbeResponsePayload != nil {
			msg.Payload = &GossipMessage_ProbeResponsePayload{ProbeResponsePayload: msg.ProbeResponsePayload}
		}
	case MessageTypeCacheSync:
		if msg.CacheSyncPayload != nil {
			msg.Payload = &GossipMessage_CacheSyncPayload{CacheSyncPayload: msg.CacheSyncPayload}
		}
	case MessageTypeCacheSyncAck:
		if msg.CacheSyncAckPayload != nil {
			msg.Payload = &GossipMessage_CacheSyncAckPayload{CacheSyncAckPayload: msg.CacheSyncAckPayload}
		}
	case MessageTypeFullSyncRequest:
		if msg.FullSyncRequestPayload != nil {
			msg.Payload = &GossipMessage_FullSyncRequestPayload{FullSyncRequestPayload: msg.FullSyncRequestPayload}
		}
	case MessageTypeFullSyncResponse:
		if msg.FullSyncResponsePayload != nil {
			msg.Payload = &GossipMessage_FullSyncResponsePayload{FullSyncResponsePayload: msg.FullSyncResponsePayload}
		}
	case MessageTypeReadRequest:
		if msg.ReadRequestPayload != nil {
			msg.Payload = &GossipMessage_ReadRequestPayload{ReadRequestPayload: msg.ReadRequestPayload}
		}
	case MessageTypeReadResponse:
		if msg.ReadResponsePayload != nil {
			msg.Payload = &GossipMessage_ReadResponsePayload{ReadResponsePayload: msg.ReadResponsePayload}
		}
	case MessageTypeBatchReadRequest:
		if msg.BatchReadRequestPayload != nil {
			msg.Payload = &GossipMessage_BatchReadRequestPayload{BatchReadRequestPayload: msg.BatchReadRequestPayload}
		}
	case MessageTypeBatchReadResponse:
		if msg.BatchReadResponsePayload != nil {
			msg.Payload = &GossipMessage_BatchReadResponsePayload{BatchReadResponsePayload: msg.BatchReadResponsePayload}
		}
	}
}

// GetPayload returns the payload interface
func (msg *GossipMessage) GetPayload() isGossipMessage_Payload {
	msg.syncPayload()
	return msg.Payload
}

// Helper methods for compatibility
func (msg *GossipMessage) GetConnectPayload() *ConnectPayload {
	return msg.ConnectPayload
}

func (msg *GossipMessage) GetClusterSyncPayload() *ClusterSyncPayload {
	return msg.ClusterSyncPayload
}

func (msg *GossipMessage) GetProbeRequestPayload() *ProbePayload {
	return msg.ProbeRequestPayload
}

func (msg *GossipMessage) GetProbeResponsePayload() *ProbeResponsePayload {
	return msg.ProbeResponsePayload
}

func (msg *GossipMessage) GetCacheSyncPayload() *SyncMessage {
	return msg.CacheSyncPayload
}

func (msg *GossipMessage) GetCacheSyncAckPayload() *CacheSyncAckPayload {
	return msg.CacheSyncAckPayload
}

func (msg *GossipMessage) GetFullSyncRequestPayload() *FullSyncRequestPayload {
	return msg.FullSyncRequestPayload
}

func (msg *GossipMessage) GetFullSyncResponsePayload() *FullSyncResponsePayload {
	return msg.FullSyncResponsePayload
}

func (msg *GossipMessage) GetReadRequestPayload() *ReadRequestPayload {
	return msg.ReadRequestPayload
}

func (msg *GossipMessage) GetReadResponsePayload() *ReadResponsePayload {
	return msg.ReadResponsePayload
}

func (msg *GossipMessage) GetBatchReadRequestPayload() *BatchReadRequestPayload {
	return msg.BatchReadRequestPayload
}

func (msg *GossipMessage) GetBatchReadResponsePayload() *BatchReadResponsePayload {
	return msg.BatchReadResponsePayload
}

// Additional getter methods for compatibility
func (msg *GossipMessage) GetType() GossipMessageType {
	return msg.Type
}

func (msg *GossipMessage) GetSender() string {
	return msg.Sender
}

func (msg *GossipMessage) GetOpId() string {
	return msg.OpId
}

func (msg *GossipMessage) GetHlc() string {
	return msg.Hlc
}

func (msg *GossipMessage) GetSignature() []byte {
	return msg.Signature
}

func (msg *GossipMessage) GetCompressed() bool {
	return msg.Compressed
}

// FullSyncPayload getters
func (p *FullSyncPayload) GetItems() []*FullStateItem {
	return p.Items
}

func (p *FullSyncPayload) GetSnapshotTimestamp() uint64 {
	return p.SnapshotTimestamp
}

// IncrementalSyncPayload getters
func (p *IncrementalSyncPayload) GetOperations() []*CacheSyncOperation {
	return p.Operations
}

func (p *IncrementalSyncPayload) GetPayloadId() string {
	return p.PayloadId
}

// CacheSyncOperation compatibility
type CacheSyncOperation_SetData struct {
	SetData *StoredItem
}

// GetDataPayload returns DataPayload for compatibility
func (op *CacheSyncOperation) GetDataPayload() interface{} {
	if op.DataPayload != nil {
		return op.DataPayload
	}
	if op.Type == OpSet && op.SetData != nil {
		return &CacheSyncOperation_SetData{SetData: op.SetData}
	}
	return nil
}

// CloneCacheSyncOperation creates a deep copy of CacheSyncOperation
func CloneCacheSyncOperation(op *CacheSyncOperation) *CacheSyncOperation {
	if op == nil {
		return nil
	}
	clone := &CacheSyncOperation{
		Key:           op.Key,
		ClientVersion: op.ClientVersion,
		Type:          op.Type,
	}
	if op.SetData != nil {
		clone.SetData = &StoredItem{
			ExpireAt: op.SetData.ExpireAt,
			Value:    make([]byte, len(op.SetData.Value)),
		}
		copy(clone.SetData.Value, op.SetData.Value)
	}
	return clone
}

// CloneGossipMessage creates a deep copy of GossipMessage
func CloneGossipMessage(msg *GossipMessage) *GossipMessage {
	if msg == nil {
		return nil
	}
	clone := &GossipMessage{
		Type:       msg.Type,
		Sender:     msg.Sender,
		OpId:       msg.OpId,
		Hlc:        msg.Hlc,
		Compressed: msg.Compressed,
	}
	if msg.Signature != nil {
		clone.Signature = make([]byte, len(msg.Signature))
		copy(clone.Signature, msg.Signature)
	}
	// Copy payload based on type
	switch msg.Type {
	case MessageTypeConnect:
		if msg.ConnectPayload != nil {
			clone.ConnectPayload = &ConnectPayload{
				NodeId:    msg.ConnectPayload.NodeId,
				Address:   msg.ConnectPayload.Address,
				Version:   msg.ConnectPayload.Version,
				Hlc:       msg.ConnectPayload.Hlc,
				PublicKey: make([]byte, len(msg.ConnectPayload.PublicKey)),
			}
			copy(clone.ConnectPayload.PublicKey, msg.ConnectPayload.PublicKey)
		}
	case MessageTypeClusterSync:
		if msg.ClusterSyncPayload != nil {
			clone.ClusterSyncPayload = &ClusterSyncPayload{
				Nodes: make([]*NodeInfo, len(msg.ClusterSyncPayload.Nodes)),
			}
			for i, n := range msg.ClusterSyncPayload.Nodes {
				clone.ClusterSyncPayload.Nodes[i] = &NodeInfo{
					NodeId:       n.NodeId,
					Address:      n.Address,
					LastActiveTs: n.LastActiveTs,
					State:        n.State,
					Version:      n.Version,
				}
			}
		}
	case MessageTypeProbeRequest:
		if msg.ProbeRequestPayload != nil {
			clone.ProbeRequestPayload = &ProbePayload{
				TargetNodeId: msg.ProbeRequestPayload.TargetNodeId,
				RequesterId:  msg.ProbeRequestPayload.RequesterId,
			}
		}
	case MessageTypeProbeResponse:
		if msg.ProbeResponsePayload != nil {
			clone.ProbeResponsePayload = &ProbeResponsePayload{
				TargetNodeId: msg.ProbeResponsePayload.TargetNodeId,
				Alive:        msg.ProbeResponsePayload.Alive,
			}
		}
	case MessageTypeCacheSync:
		if msg.CacheSyncPayload != nil {
			clone.CacheSyncPayload = &SyncMessage{}
			if msg.CacheSyncPayload.IncrementalSync != nil {
				clone.CacheSyncPayload.IncrementalSync = &IncrementalSyncPayload{
					PayloadId:  msg.CacheSyncPayload.IncrementalSync.PayloadId,
					Operations: make([]*CacheSyncOperation, len(msg.CacheSyncPayload.IncrementalSync.Operations)),
				}
				for i, op := range msg.CacheSyncPayload.IncrementalSync.Operations {
					clone.CacheSyncPayload.IncrementalSync.Operations[i] = CloneCacheSyncOperation(op)
				}
			}
			if msg.CacheSyncPayload.FullSync != nil {
				clone.CacheSyncPayload.FullSync = &FullSyncPayload{
					SnapshotTimestamp: msg.CacheSyncPayload.FullSync.SnapshotTimestamp,
					Items:             make([]*FullStateItem, len(msg.CacheSyncPayload.FullSync.Items)),
				}
				for i, item := range msg.CacheSyncPayload.FullSync.Items {
					clone.CacheSyncPayload.FullSync.Items[i] = &FullStateItem{
						Key:     item.Key,
						Version: item.Version,
					}
					if item.ItemData != nil {
						clone.CacheSyncPayload.FullSync.Items[i].ItemData = &StoredItem{
							ExpireAt: item.ItemData.ExpireAt,
							Value:    make([]byte, len(item.ItemData.Value)),
						}
						copy(clone.CacheSyncPayload.FullSync.Items[i].ItemData.Value, item.ItemData.Value)
					}
				}
			}
		}
	}
	return clone
}

// Compatibility constants for OperationType
const (
	OperationType_OP_UNSPECIFIED OperationType = OpUnspecified
	OperationType_OP_SET         OperationType = OpSet
	OperationType_OP_DELETE      OperationType = OpDelete
)

// Compatibility type aliases
const (
	NodeState_NODE_STATE_UNKNOWN NodeState = NodeStateUnknown
	NodeState_NODE_STATE_ALIVE   NodeState = NodeStateAlive
	NodeState_NODE_STATE_SUSPECT NodeState = NodeStateSuspect
	NodeState_NODE_STATE_DEAD    NodeState = NodeStateDead
)

const (
	GossipMessageType_MESSAGE_TYPE_UNKNOWN             GossipMessageType = MessageTypeUnknown
	GossipMessageType_MESSAGE_TYPE_CACHE_SYNC          GossipMessageType = MessageTypeCacheSync
	GossipMessageType_MESSAGE_TYPE_CLUSTER_SYNC        GossipMessageType = MessageTypeClusterSync
	GossipMessageType_MESSAGE_TYPE_CONNECT             GossipMessageType = MessageTypeConnect
	GossipMessageType_MESSAGE_TYPE_PROBE_REQUEST       GossipMessageType = MessageTypeProbeRequest
	GossipMessageType_MESSAGE_TYPE_PROBE_RESPONSE      GossipMessageType = MessageTypeProbeResponse
	GossipMessageType_MESSAGE_TYPE_CACHE_SYNC_ACK      GossipMessageType = MessageTypeCacheSyncAck
	GossipMessageType_MESSAGE_TYPE_FULL_SYNC_REQUEST   GossipMessageType = MessageTypeFullSyncRequest
	GossipMessageType_MESSAGE_TYPE_FULL_SYNC_RESPONSE  GossipMessageType = MessageTypeFullSyncResponse
	GossipMessageType_MESSAGE_TYPE_READ_REQUEST        GossipMessageType = MessageTypeReadRequest
	GossipMessageType_MESSAGE_TYPE_READ_RESPONSE       GossipMessageType = MessageTypeReadResponse
	GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST  GossipMessageType = MessageTypeBatchReadRequest
	GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE GossipMessageType = MessageTypeBatchReadResponse
)
