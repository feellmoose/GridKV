package gossip

import (
	"encoding/binary"
	"errors"
	"sync"

	"github.com/golang/snappy"
)

var (
	ErrInvalidMessage = errors.New("invalid binary message")
	binaryMessagePool = sync.Pool{
		New: func() interface{} {
			return &BinaryMessage{
				Payload: make([]byte, 0, 4096),
			}
		},
	}
)

const (
	BinaryMsgTypeCacheSync     = 1
	BinaryMsgTypeReadReq       = 2
	BinaryMsgTypeReadResp      = 3
	BinaryMsgTypeClusterSync   = 4
	BinaryMsgTypeConnect       = 5
	BinaryMsgTypeProbeReq      = 6
	BinaryMsgTypeProbeResp     = 7
	BinaryMsgTypeBatchReadReq  = 8
	BinaryMsgTypeBatchReadResp = 9
)

type BinaryMessage struct {
	Type    uint8
	Sender  [16]byte
	Payload []byte
}

func GetBinaryMessage() *BinaryMessage {
	msg := binaryMessagePool.Get().(*BinaryMessage)
	msg.Payload = msg.Payload[:0]
	return msg
}

func PutBinaryMessage(msg *BinaryMessage) {
	if cap(msg.Payload) > 65536 {
		return
	}
	binaryMessagePool.Put(msg)
}

func (bm *BinaryMessage) Marshal() []byte {
	buf := make([]byte, 21+len(bm.Payload))
	buf[0] = bm.Type
	copy(buf[1:17], bm.Sender[:])
	binary.LittleEndian.PutUint32(buf[17:21], uint32(len(bm.Payload)))
	copy(buf[21:], bm.Payload)
	return buf
}

func UnmarshalBinaryMessage(data []byte) (*BinaryMessage, error) {
	if len(data) < 21 {
		return nil, ErrInvalidMessage
	}
	msg := &BinaryMessage{
		Type:    data[0],
		Payload: data[21:],
	}
	copy(msg.Sender[:], data[1:17])
	return msg, nil
}

// convertBinaryToGossipMessage converts BinaryMessage to GossipMessage
const (
	// Stage 2.5: Reduced threshold from 128KB to 32KB to expand compression coverage
	cacheSyncCompressThreshold = 32 * 1024
	cacheSyncCompressedMagic   = 0xEACEEA5E
)

func convertBinaryToGossipMessage(binary *BinaryMessage) *GossipMessage {
	if binary == nil {
		return nil
	}

	msg := getGossipMessage()

	senderLen := 0
	for i := 0; i < len(binary.Sender); i++ {
		if binary.Sender[i] == 0 {
			break
		}
		senderLen = i + 1
	}
	msg.Sender = string(binary.Sender[:senderLen])

	switch binary.Type {
	case BinaryMsgTypeCacheSync:
		msg.Type = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC
		ops, compressed, err := decodeCacheSyncPayload(binary.Payload)
		if err == nil && len(ops) > 0 {
			syncMsg := &SyncMessage{
				IncrementalSync: &IncrementalSyncPayload{
					Operations: ops,
				},
			}
			msg.Compressed = compressed
			msg.CacheSyncPayload = syncMsg
			msg.Payload = &GossipMessage_CacheSyncPayload{
				CacheSyncPayload: syncMsg,
			}
		}
	case BinaryMsgTypeConnect:
		msg.Type = GossipMessageType_MESSAGE_TYPE_CONNECT
		payload, err := DecodeConnectPayload(binary.Payload)
		if err == nil && payload != nil {
			msg.ConnectPayload = payload
			msg.Payload = &GossipMessage_ConnectPayload{
				ConnectPayload: payload,
			}
		}
	case BinaryMsgTypeClusterSync:
		msg.Type = GossipMessageType_MESSAGE_TYPE_CLUSTER_SYNC
		payload, err := DecodeClusterSyncPayload(binary.Payload)
		if err == nil && payload != nil {
			msg.ClusterSyncPayload = payload
			msg.Payload = &GossipMessage_ClusterSyncPayload{
				ClusterSyncPayload: payload,
			}
		}
	case BinaryMsgTypeProbeReq:
		msg.Type = GossipMessageType_MESSAGE_TYPE_PROBE_REQUEST
		payload, err := DecodeProbePayload(binary.Payload)
		if err == nil && payload != nil {
			msg.ProbeRequestPayload = payload
			msg.Payload = &GossipMessage_ProbeRequestPayload{
				ProbeRequestPayload: payload,
			}
		}
	case BinaryMsgTypeProbeResp:
		msg.Type = GossipMessageType_MESSAGE_TYPE_PROBE_RESPONSE
		payload, err := DecodeProbeResponsePayload(binary.Payload)
		if err == nil && payload != nil {
			msg.ProbeResponsePayload = payload
			msg.Payload = &GossipMessage_ProbeResponsePayload{
				ProbeResponsePayload: payload,
			}
		}
	case BinaryMsgTypeReadReq:
		msg.Type = GossipMessageType_MESSAGE_TYPE_READ_REQUEST
	case BinaryMsgTypeReadResp:
		msg.Type = GossipMessageType_MESSAGE_TYPE_READ_RESPONSE
	case BinaryMsgTypeBatchReadReq:
		msg.Type = GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST
	case BinaryMsgTypeBatchReadResp:
		msg.Type = GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE
	default:
		putGossipMessage(msg)
		return nil
	}

	return msg
}

// convertGossipMessageToBinary converts GossipMessage to BinaryMessage
func convertGossipMessageToBinary(msg *GossipMessage) *BinaryMessage {
	if msg == nil {
		return nil
	}

	binary := GetBinaryMessage()

	senderBytes := []byte(msg.Sender)
	copy(binary.Sender[:], senderBytes)
	if len(senderBytes) < 16 {
		for i := len(senderBytes); i < 16; i++ {
			binary.Sender[i] = 0
		}
	}

	switch msg.Type {
	case GossipMessageType_MESSAGE_TYPE_CACHE_SYNC:
		binary.Type = BinaryMsgTypeCacheSync
		var syncMsg *SyncMessage
		if msg.CacheSyncPayload != nil {
			syncMsg = msg.CacheSyncPayload
		} else if p, ok := msg.Payload.(*GossipMessage_CacheSyncPayload); ok && p.CacheSyncPayload != nil {
			syncMsg = p.CacheSyncPayload
		}
		if syncMsg != nil {
			if incSync := syncMsg.GetIncrementalSync(); incSync != nil {
				payload, _ := encodeCacheSyncPayload(incSync.Operations)
				binary.Payload = payload
			}
		}
	case GossipMessageType_MESSAGE_TYPE_CONNECT:
		binary.Type = BinaryMsgTypeConnect
		var payload *ConnectPayload
		if msg.ConnectPayload != nil {
			payload = msg.ConnectPayload
		} else if p, ok := msg.Payload.(*GossipMessage_ConnectPayload); ok && p.ConnectPayload != nil {
			payload = p.ConnectPayload
		}
		if payload != nil {
			binary.Payload = EncodeConnectPayload(payload)
		}
	case GossipMessageType_MESSAGE_TYPE_CLUSTER_SYNC:
		binary.Type = BinaryMsgTypeClusterSync
		var payload *ClusterSyncPayload
		if msg.ClusterSyncPayload != nil {
			payload = msg.ClusterSyncPayload
		} else if p, ok := msg.Payload.(*GossipMessage_ClusterSyncPayload); ok && p.ClusterSyncPayload != nil {
			payload = p.ClusterSyncPayload
		}
		if payload != nil {
			binary.Payload = EncodeClusterSyncPayload(payload)
		}
	case GossipMessageType_MESSAGE_TYPE_PROBE_REQUEST:
		binary.Type = BinaryMsgTypeProbeReq
		var payload *ProbePayload
		if msg.ProbeRequestPayload != nil {
			payload = msg.ProbeRequestPayload
		} else if p, ok := msg.Payload.(*GossipMessage_ProbeRequestPayload); ok && p.ProbeRequestPayload != nil {
			payload = p.ProbeRequestPayload
		}
		if payload != nil {
			binary.Payload = EncodeProbePayload(payload)
		}
	case GossipMessageType_MESSAGE_TYPE_PROBE_RESPONSE:
		binary.Type = BinaryMsgTypeProbeResp
		var payload *ProbeResponsePayload
		if msg.ProbeResponsePayload != nil {
			payload = msg.ProbeResponsePayload
		} else if p, ok := msg.Payload.(*GossipMessage_ProbeResponsePayload); ok && p.ProbeResponsePayload != nil {
			payload = p.ProbeResponsePayload
		}
		if payload != nil {
			binary.Payload = EncodeProbeResponsePayload(payload)
		}
	case GossipMessageType_MESSAGE_TYPE_READ_REQUEST:
		binary.Type = BinaryMsgTypeReadReq
	case GossipMessageType_MESSAGE_TYPE_READ_RESPONSE:
		binary.Type = BinaryMsgTypeReadResp
	case GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST:
		binary.Type = BinaryMsgTypeBatchReadReq
	case GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE:
		binary.Type = BinaryMsgTypeBatchReadResp
	default:
		PutBinaryMessage(binary)
		return nil
	}

	return binary
}

// maxSerializedMessageSize is the maximum size for a serialized message before sending
// Set to 9MB to leave room for protocol headers and avoid TCP 10MB limit
const maxSerializedMessageSize = 9 * 1024 * 1024

// EncodeOperations encodes CacheSyncOperation slice to binary format
func encodeCacheSyncPayload(ops []*CacheSyncOperation) ([]byte, bool) {
	if len(ops) == 0 {
		return nil, false
	}
	raw := EncodeOperations(ops)
	if len(raw) <= cacheSyncCompressThreshold {
		return raw, false
	}
	compressed := snappy.Encode(nil, raw)
	payload := make([]byte, 4+len(compressed))
	binary.LittleEndian.PutUint32(payload[0:4], cacheSyncCompressedMagic)
	copy(payload[4:], compressed)
	return payload, true
}

func decodeCacheSyncPayload(data []byte) ([]*CacheSyncOperation, bool, error) {
	if len(data) < 4 {
		return nil, false, ErrInvalidMessage
	}
	if binary.LittleEndian.Uint32(data[0:4]) != cacheSyncCompressedMagic {
		ops, err := DecodeOperations(data)
		return ops, false, err
	}
	decompressed, err := snappy.Decode(nil, data[4:])
	if err != nil {
		return nil, false, err
	}
	ops, err := DecodeOperations(decompressed)
	return ops, true, err
}

func EncodeOperations(ops []*CacheSyncOperation) []byte {
	if len(ops) == 0 {
		return nil
	}

	buf := make([]byte, 0, len(ops)*256)
	buf = append(buf, make([]byte, 4)...)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(ops)))

	for _, op := range ops {
		opBuf := encodeOperation(op)
		opLen := make([]byte, 4)
		binary.LittleEndian.PutUint32(opLen, uint32(len(opBuf)))
		buf = append(buf, opLen...)
		buf = append(buf, opBuf...)
	}
	return buf
}

func encodeOperation(op *CacheSyncOperation) []byte {
	if op == nil {
		return nil
	}

	keyBytes := []byte(op.Key)
	estimatedSize := 2 + len(keyBytes) + 8 + 1
	if op.Type == OperationType_OP_SET && op.GetSetData() != nil {
		setData := op.GetSetData()
		estimatedSize += 4 + 8 + 4 + len(setData.Value)
	}

	buf := make([]byte, 0, estimatedSize)

	// Key length (2 bytes)
	buf = append(buf, byte(len(keyBytes)&0xFF), byte(len(keyBytes)>>8))
	buf = append(buf, keyBytes...)

	// Version (8 bytes)
	versionBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(versionBytes, uint64(op.ClientVersion))
	buf = append(buf, versionBytes...)

	// Type (1 byte)
	buf = append(buf, byte(op.Type))

	if op.Type == OperationType_OP_SET && op.GetSetData() != nil {
		setData := op.GetSetData()
		itemBuf := encodeStoredItem(setData)
		// Item length (4 bytes)
		itemLen := uint32(len(itemBuf))
		buf = append(buf, byte(itemLen&0xFF), byte(itemLen>>8), byte(itemLen>>16), byte(itemLen>>24))
		buf = append(buf, itemBuf...)
	}

	return buf
}

func encodeStoredItem(item *StoredItem) []byte {
	if item == nil {
		return nil
	}

	buf := make([]byte, 8+4+len(item.Value))
	binary.LittleEndian.PutUint64(buf[0:8], item.ExpireAt)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(len(item.Value)))
	copy(buf[12:], item.Value)
	return buf
}

// DecodeOperations decodes binary data to CacheSyncOperation slice
func DecodeOperations(data []byte) ([]*CacheSyncOperation, error) {
	if len(data) < 4 {
		return nil, ErrInvalidMessage
	}

	count := binary.LittleEndian.Uint32(data[0:4])
	ops := make([]*CacheSyncOperation, 0, count)
	offset := 4

	for i := uint32(0); i < count; i++ {
		if offset+4 > len(data) {
			return nil, ErrInvalidMessage
		}

		opLen := binary.LittleEndian.Uint32(data[offset : offset+4])
		offset += 4

		if offset+int(opLen) > len(data) {
			return nil, ErrInvalidMessage
		}

		op, err := decodeOperation(data[offset : offset+int(opLen)])
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
		offset += int(opLen)
	}

	return ops, nil
}

func decodeOperation(data []byte) (*CacheSyncOperation, error) {
	if len(data) < 2 {
		return nil, ErrInvalidMessage
	}

	offset := 0
	keyLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2

	if offset+int(keyLen) > len(data) {
		return nil, ErrInvalidMessage
	}
	key := string(data[offset : offset+int(keyLen)])
	offset += int(keyLen)

	if offset+8 > len(data) {
		return nil, ErrInvalidMessage
	}
	version := int64(binary.LittleEndian.Uint64(data[offset : offset+8]))
	offset += 8

	if offset >= len(data) {
		return nil, ErrInvalidMessage
	}
	opType := OperationType(data[offset])
	offset++

	op := &CacheSyncOperation{
		Key:           key,
		ClientVersion: version,
		Type:          opType,
	}

	if opType == OperationType_OP_SET && offset < len(data) {
		if offset+4 > len(data) {
			return nil, ErrInvalidMessage
		}
		itemLen := binary.LittleEndian.Uint32(data[offset : offset+4])
		offset += 4

		if offset+int(itemLen) > len(data) {
			return nil, ErrInvalidMessage
		}
		item, err := decodeStoredItem(data[offset : offset+int(itemLen)])
		if err != nil {
			return nil, err
		}
		op.SetData = item
		op.DataPayload = &CacheSyncOperation_SetData{
			SetData: item,
		}
	}

	return op, nil
}

func decodeStoredItem(data []byte) (*StoredItem, error) {
	if len(data) < 12 {
		return nil, ErrInvalidMessage
	}

	expireAt := binary.LittleEndian.Uint64(data[0:8])
	valueLen := binary.LittleEndian.Uint32(data[8:12])

	if 12+int(valueLen) > len(data) {
		return nil, ErrInvalidMessage
	}

	value := make([]byte, valueLen)
	copy(value, data[12:12+int(valueLen)])

	return &StoredItem{
		ExpireAt: expireAt,
		Value:    value,
	}, nil
}

// EncodeConnectPayload encodes ConnectPayload to binary format
func EncodeConnectPayload(payload *ConnectPayload) []byte {
	if payload == nil {
		return nil
	}
	nodeIDBytes := []byte(payload.NodeId)
	addrBytes := []byte(payload.Address)
	hlcBytes := []byte(payload.Hlc)

	buf := make([]byte, 0, 2+len(nodeIDBytes)+2+len(addrBytes)+8+2+len(hlcBytes)+2+len(payload.PublicKey))

	// NodeId (2 bytes length + data)
	buf = append(buf, byte(len(nodeIDBytes)&0xFF), byte(len(nodeIDBytes)>>8))
	buf = append(buf, nodeIDBytes...)

	// Address (2 bytes length + data)
	buf = append(buf, byte(len(addrBytes)&0xFF), byte(len(addrBytes)>>8))
	buf = append(buf, addrBytes...)

	// Version (8 bytes)
	versionBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(versionBytes, uint64(payload.Version))
	buf = append(buf, versionBytes...)

	// HLC (2 bytes length + data)
	buf = append(buf, byte(len(hlcBytes)&0xFF), byte(len(hlcBytes)>>8))
	buf = append(buf, hlcBytes...)

	// PublicKey (2 bytes length + data)
	buf = append(buf, byte(len(payload.PublicKey)&0xFF), byte(len(payload.PublicKey)>>8))
	buf = append(buf, payload.PublicKey...)

	return buf
}

// DecodeConnectPayload decodes binary data to ConnectPayload
func DecodeConnectPayload(data []byte) (*ConnectPayload, error) {
	if len(data) < 2 {
		return nil, ErrInvalidMessage
	}

	offset := 0

	// NodeId
	if offset+2 > len(data) {
		return nil, ErrInvalidMessage
	}
	nodeIDLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2
	if offset+int(nodeIDLen) > len(data) {
		return nil, ErrInvalidMessage
	}
	nodeID := string(data[offset : offset+int(nodeIDLen)])
	offset += int(nodeIDLen)

	// Address
	if offset+2 > len(data) {
		return nil, ErrInvalidMessage
	}
	addrLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2
	if offset+int(addrLen) > len(data) {
		return nil, ErrInvalidMessage
	}
	address := string(data[offset : offset+int(addrLen)])
	offset += int(addrLen)

	// Version
	if offset+8 > len(data) {
		return nil, ErrInvalidMessage
	}
	version := int64(binary.LittleEndian.Uint64(data[offset : offset+8]))
	offset += 8

	// HLC
	if offset+2 > len(data) {
		return nil, ErrInvalidMessage
	}
	hlcLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2
	hlc := ""
	if hlcLen > 0 {
		if offset+int(hlcLen) > len(data) {
			return nil, ErrInvalidMessage
		}
		hlc = string(data[offset : offset+int(hlcLen)])
		offset += int(hlcLen)
	}

	// PublicKey
	publicKey := []byte(nil)
	if offset+2 <= len(data) {
		pubKeyLen := binary.LittleEndian.Uint16(data[offset : offset+2])
		offset += 2
		if offset+int(pubKeyLen) <= len(data) {
			publicKey = make([]byte, pubKeyLen)
			copy(publicKey, data[offset:offset+int(pubKeyLen)])
		}
	}

	return &ConnectPayload{
		NodeId:    nodeID,
		Address:   address,
		Version:   version,
		Hlc:       hlc,
		PublicKey: publicKey,
	}, nil
}

// EncodeClusterSyncPayload encodes ClusterSyncPayload to binary format
func EncodeClusterSyncPayload(payload *ClusterSyncPayload) []byte {
	if payload == nil || len(payload.Nodes) == 0 {
		return nil
	}

	buf := make([]byte, 0, 4+len(payload.Nodes)*128)

	// Node count (4 bytes)
	countBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(countBytes, uint32(len(payload.Nodes)))
	buf = append(buf, countBytes...)

	for _, node := range payload.Nodes {
		nodeBuf := encodeNodeInfo(node)
		nodeLen := make([]byte, 4)
		binary.LittleEndian.PutUint32(nodeLen, uint32(len(nodeBuf)))
		buf = append(buf, nodeLen...)
		buf = append(buf, nodeBuf...)
	}

	return buf
}

func encodeNodeInfo(node *NodeInfo) []byte {
	if node == nil {
		return nil
	}

	nodeIDBytes := []byte(node.NodeId)
	addrBytes := []byte(node.Address)

	buf := make([]byte, 0, 2+len(nodeIDBytes)+2+len(addrBytes)+8+1+8)

	// NodeId
	buf = append(buf, byte(len(nodeIDBytes)&0xFF), byte(len(nodeIDBytes)>>8))
	buf = append(buf, nodeIDBytes...)

	// Address
	buf = append(buf, byte(len(addrBytes)&0xFF), byte(len(addrBytes)>>8))
	buf = append(buf, addrBytes...)

	// State (1 byte)
	buf = append(buf, byte(node.State))

	// Version (8 bytes)
	versionBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(versionBytes, uint64(node.Version))
	buf = append(buf, versionBytes...)

	return buf
}

// DecodeClusterSyncPayload decodes binary data to ClusterSyncPayload
func DecodeClusterSyncPayload(data []byte) (*ClusterSyncPayload, error) {
	if len(data) < 4 {
		return nil, ErrInvalidMessage
	}

	count := binary.LittleEndian.Uint32(data[0:4])
	nodes := make([]*NodeInfo, 0, count)
	offset := 4

	for i := uint32(0); i < count; i++ {
		if offset+4 > len(data) {
			return nil, ErrInvalidMessage
		}

		nodeLen := binary.LittleEndian.Uint32(data[offset : offset+4])
		offset += 4

		if offset+int(nodeLen) > len(data) {
			return nil, ErrInvalidMessage
		}

		node, err := decodeNodeInfo(data[offset : offset+int(nodeLen)])
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
		offset += int(nodeLen)
	}

	return &ClusterSyncPayload{Nodes: nodes}, nil
}

func decodeNodeInfo(data []byte) (*NodeInfo, error) {
	if len(data) < 2 {
		return nil, ErrInvalidMessage
	}

	offset := 0

	// NodeId
	nodeIDLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2
	if offset+int(nodeIDLen) > len(data) {
		return nil, ErrInvalidMessage
	}
	nodeID := string(data[offset : offset+int(nodeIDLen)])
	offset += int(nodeIDLen)

	// Address
	if offset+2 > len(data) {
		return nil, ErrInvalidMessage
	}
	addrLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2
	if offset+int(addrLen) > len(data) {
		return nil, ErrInvalidMessage
	}
	address := string(data[offset : offset+int(addrLen)])
	offset += int(addrLen)

	// State
	if offset >= len(data) {
		return nil, ErrInvalidMessage
	}
	state := NodeState(data[offset])
	offset++

	// Version
	if offset+8 > len(data) {
		return nil, ErrInvalidMessage
	}
	version := int64(binary.LittleEndian.Uint64(data[offset : offset+8]))

	return &NodeInfo{
		NodeId:  nodeID,
		Address: address,
		State:   state,
		Version: version,
	}, nil
}

// EncodeProbePayload encodes ProbePayload to binary format
func EncodeProbePayload(payload *ProbePayload) []byte {
	if payload == nil {
		return nil
	}

	targetBytes := []byte(payload.TargetNodeId)
	requesterBytes := []byte(payload.RequesterId)

	buf := make([]byte, 0, 2+len(targetBytes)+2+len(requesterBytes))

	// TargetNodeId
	buf = append(buf, byte(len(targetBytes)&0xFF), byte(len(targetBytes)>>8))
	buf = append(buf, targetBytes...)

	// RequesterId
	buf = append(buf, byte(len(requesterBytes)&0xFF), byte(len(requesterBytes)>>8))
	buf = append(buf, requesterBytes...)

	return buf
}

// DecodeProbePayload decodes binary data to ProbePayload
func DecodeProbePayload(data []byte) (*ProbePayload, error) {
	if len(data) < 2 {
		return nil, ErrInvalidMessage
	}

	offset := 0

	// TargetNodeId
	targetLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2
	if offset+int(targetLen) > len(data) {
		return nil, ErrInvalidMessage
	}
	target := string(data[offset : offset+int(targetLen)])
	offset += int(targetLen)

	// RequesterId
	if offset+2 > len(data) {
		return nil, ErrInvalidMessage
	}
	requesterLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2
	if offset+int(requesterLen) > len(data) {
		return nil, ErrInvalidMessage
	}
	requester := string(data[offset : offset+int(requesterLen)])

	return &ProbePayload{
		TargetNodeId: target,
		RequesterId:  requester,
	}, nil
}

// EncodeProbeResponsePayload encodes ProbeResponsePayload to binary format
func EncodeProbeResponsePayload(payload *ProbeResponsePayload) []byte {
	if payload == nil {
		return nil
	}

	targetBytes := []byte(payload.TargetNodeId)

	buf := make([]byte, 0, 2+len(targetBytes)+1)

	// TargetNodeId
	buf = append(buf, byte(len(targetBytes)&0xFF), byte(len(targetBytes)>>8))
	buf = append(buf, targetBytes...)

	// Alive (1 byte)
	if payload.Alive {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}

	return buf
}

// DecodeProbeResponsePayload decodes binary data to ProbeResponsePayload
func DecodeProbeResponsePayload(data []byte) (*ProbeResponsePayload, error) {
	if len(data) < 2 {
		return nil, ErrInvalidMessage
	}

	offset := 0

	// TargetNodeId
	targetLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2
	if offset+int(targetLen) > len(data) {
		return nil, ErrInvalidMessage
	}
	target := string(data[offset : offset+int(targetLen)])
	offset += int(targetLen)

	// Alive
	if offset >= len(data) {
		return nil, ErrInvalidMessage
	}
	alive := data[offset] != 0

	return &ProbeResponsePayload{
		TargetNodeId: target,
		Alive:        alive,
	}, nil
}
