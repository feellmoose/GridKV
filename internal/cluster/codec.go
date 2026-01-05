package cluster

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/zerocopy"
)

// Buffer pools for encoding/decoding
var (
	// Small buffer pool (64-128 bytes) for message encoding
	smallBufPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 0, 128)
		},
	}

	// Medium buffer pool (256-512 bytes) for larger messages
	mediumBufPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 0, 512)
		},
	}

	// Fixed-size buffer pool (8 bytes) for incarnation/timestamp encoding
	fixed8BufPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 8)
		},
	}

	// Serialization buffer pool for SyncOps (4KB initial capacity)
	serializeBufPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 0, 4096)
		},
	}
)

// Note: We don't use buffer pools here because we return the buffer to the caller
// The caller owns the buffer and may modify it. Using pools would require copying

// SyncOpsCodec handles serialization/deserialization of SyncOperation slices
type SyncOpsCodec struct{}

// Serialize serializes sync operations to bytes
// Uses zerocopy for zero-allocation where possible
func (c *SyncOpsCodec) Serialize(ops []*mem_storage.SyncOperation) ([]byte, error) {
	if len(ops) == 0 {
		return nil, nil
	}

	// Estimate size: 4 (count) + per-op: 2 (key len) + key + 8 (version) + 1 (type) + 8 (expire) + 4 (value len) + value
	estimatedSize := 4
	for _, op := range ops {
		estimatedSize += 2 + len(op.Key) + 8 + 1 + 8 + 4
		if op.Item != nil && op.Item.Value != nil {
			estimatedSize += len(op.Item.Value)
		}
	}

	// Allocate buffer (we'll return it, so don't use pool directly)
	buf := make([]byte, 0, estimatedSize)

	// Count (4 bytes)
	buf = append(buf, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(buf[len(buf)-4:], uint32(len(ops)))

	for _, op := range ops {
		opBuf := c.encodeOp(op)
		if opBuf == nil {
			continue
		}
		// Op length (4 bytes)
		buf = append(buf, 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(buf[len(buf)-4:], uint32(len(opBuf)))
		buf = append(buf, opBuf...)
	}

	return buf, nil
}

// Deserialize deserializes bytes to sync operations
func (c *SyncOpsCodec) Deserialize(data []byte) ([]*mem_storage.SyncOperation, error) {
	if len(data) < 4 {
		return nil, nil
	}

	count := binary.LittleEndian.Uint32(data[0:4])
	ops := make([]*mem_storage.SyncOperation, 0, count)
	offset := 4

	for i := uint32(0); i < count; i++ {
		if offset+4 > len(data) {
			break
		}

		opLen := binary.LittleEndian.Uint32(data[offset : offset+4])
		offset += 4

		if offset+int(opLen) > len(data) {
			break
		}

		op, err := c.decodeOp(data[offset : offset+int(opLen)])
		if err != nil {
			continue
		}
		ops = append(ops, op)
		offset += int(opLen)
	}

	return ops, nil
}

// encodeOp encodes single operation
// Uses zerocopy for key conversion
func (c *SyncOpsCodec) encodeOp(op *mem_storage.SyncOperation) []byte {
	if op == nil || op.Item == nil {
		return nil
	}

	// Use zerocopy for key (read-only, safe)
	keyBytes := zerocopy.StringToBytes(op.Key)
	valueLen := 0
	expireAt := int64(0)
	if op.Item.Value != nil {
		valueLen = len(op.Item.Value)
	}
	if !op.Item.ExpireAt.IsZero() {
		expireAt = op.Item.ExpireAt.UnixNano()
	}

	estimatedSize := 2 + len(keyBytes) + 8 + 1 + 8 + 4 + valueLen
	buf := make([]byte, 0, estimatedSize)

	// Key length (2 bytes) - use zerocopy for key bytes (read-only)
	buf = append(buf, byte(len(keyBytes)&0xFF), byte(len(keyBytes)>>8))
	buf = append(buf, keyBytes...)

	// Version (8 bytes)
	buf = append(buf, 0, 0, 0, 0, 0, 0, 0, 0)
	binary.LittleEndian.PutUint64(buf[len(buf)-8:], uint64(op.Item.Version))

	// OpType (1 byte: 0=Set, 1=Delete)
	opType := byte(0) // OpSet
	if op.OpType == mem_storage.OpDelete {
		opType = 1
	}
	buf = append(buf, opType)

	// ExpireAt (8 bytes)
	buf = append(buf, 0, 0, 0, 0, 0, 0, 0, 0)
	binary.LittleEndian.PutUint64(buf[len(buf)-8:], uint64(expireAt))

	// Value length (4 bytes) + value
	buf = append(buf, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(buf[len(buf)-4:], uint32(valueLen))
	if valueLen > 0 && op.Item.Value != nil {
		// Append value directly (buf owns the data, safe)
		buf = append(buf, op.Item.Value...)
	}

	return buf
}

// decodeOp decodes single operation
// Uses zerocopy for string conversion where safe
func (c *SyncOpsCodec) decodeOp(data []byte) (*mem_storage.SyncOperation, error) {
	if len(data) < 2 {
		return nil, nil
	}

	offset := 0

	// Key length (2 bytes)
	keyLen := binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2

	if offset+int(keyLen) > len(data) {
		return nil, nil
	}
	// Use zerocopy for key (safe: we create new string)
	key := zerocopy.BytesToString(data[offset : offset+int(keyLen)])
	offset += int(keyLen)

	if offset+8 > len(data) {
		return nil, nil
	}
	version := int64(binary.LittleEndian.Uint64(data[offset : offset+8]))
	offset += 8

	if offset >= len(data) {
		return nil, nil
	}
	opType := mem_storage.OpType(data[offset])
	offset++

	if offset+8 > len(data) {
		return nil, nil
	}
	expireAt := int64(binary.LittleEndian.Uint64(data[offset : offset+8]))
	offset += 8

	if offset+4 > len(data) {
		return nil, nil
	}
	valueLen := binary.LittleEndian.Uint32(data[offset : offset+4])
	offset += 4

	var value []byte
	if valueLen > 0 {
		if offset+int(valueLen) > len(data) {
			return nil, nil
		}
		// Clone value for safety (caller may modify)
		value = zerocopy.FastCloneBytes(data[offset : offset+int(valueLen)])
	}

	var expireTime time.Time
	if expireAt > 0 {
		expireTime = time.Unix(0, expireAt)
	}

	item := &mem_storage.StoredItem{
		Version:  version,
		ExpireAt: expireTime,
		Value:    value,
		Key:      key,
	}

	return &mem_storage.SyncOperation{
		Key:    key,
		OpType: opType,
		Item:   item,
	}, nil
}

// Global codec instance (thread-safe, stateless)
var syncOpsCodec = &SyncOpsCodec{}

// SerializeSyncOps serializes sync operations (convenience function)
func SerializeSyncOps(ops []*mem_storage.SyncOperation) ([]byte, error) {
	return syncOpsCodec.Serialize(ops)
}

// DeserializeSyncOps deserializes sync operations (convenience function)
func DeserializeSyncOps(data []byte) ([]*mem_storage.SyncOperation, error) {
	return syncOpsCodec.Deserialize(data)
}

// MemberMsgCodec handles serialization/deserialization of SWIM member messages
// Binary format, no reflection
type MemberMsgCodec struct{}

// EncodeMemberMsg encodes member message to bytes (binary format, no reflection)
func (c *MemberMsgCodec) EncodeMemberMsg(msg interface{}) ([]byte, error) {
	switch v := msg.(type) {
	case *pingMsg:
		return c.encodePingMsg(v), nil
	case *ackMsg:
		return c.encodeAckMsg(v), nil
	case *connectMsg:
		return c.encodeConnectMsg(v), nil
	case *leaveMsg:
		return c.encodeLeaveMsg(v), nil
	case *indirectProbeMsg:
		return c.encodeIndirectProbeMsg(v), nil
	case *clusterSyncMsg:
		return c.encodeClusterSyncMsg(v)
	default:
		return nil, nil
	}
}

// DecodeMemberMsg decodes member message from bytes (binary format, no reflection)
// msgType: 1=Ping, 2=Connect, 3=Leave (internal mapping from network.MessageType)
func (c *MemberMsgCodec) DecodeMemberMsg(data []byte, msgType uint8) interface{} {
	if len(data) == 0 {
		return nil
	}
	switch msgType {
	case 1: // Ping (also used for ACK and IndirectProbe)
		// Try ping first (has To field)
		if msg := c.decodePingMsg(data); msg != nil && msg.To != "" {
			return msg
		}
		// Try ack (has To field)
		if msg := c.decodeAckMsg(data); msg != nil && msg.To != "" {
			return msg
		}
		// Try indirect probe (has Target field)
		return c.decodeIndirectProbeMsg(data)
	case 2: // Connect (also used for ClusterSync)
		// Distinguish by structure:
		// - connectMsg: NodeID (string) + Address (string) + Incarnation (8 bytes)
		// - clusterSyncMsg: From (string) + memberCount (4 bytes) + Members (NodeInfo array)
		// If after first string we see a 4-byte value that looks like a count (small positive), it's clusterSync
		// Otherwise, try connectMsg first
		if len(data) >= 10 {
			offset := 0
			_, newOffset := c.decodeString(data, offset)
			if newOffset > 0 && newOffset+4 <= len(data) {
				memberCount := int(binary.LittleEndian.Uint32(data[newOffset:]))
				// If it looks like a reasonable member count (0-1000), it's likely clusterSync
				if memberCount >= 0 && memberCount <= 1000 {
					return c.decodeClusterSyncMsg(data)
				}
			}
		}
		// Try connect first (has NodeID and Address)
		connectMsg := c.decodeConnectMsg(data)
		if connectMsg != nil {
			if connectMsg.NodeID != "" && connectMsg.Address != "" {
				if len(connectMsg.Address) < 3 || connectMsg.Address[0] == 0 {
					return nil
				}
				return connectMsg
			}
		}
		// Try cluster sync as fallback
		return c.decodeClusterSyncMsg(data)
	case 3: // Leave
		return c.decodeLeaveMsg(data)
	}
	return nil
}

// Binary encoding helpers (no reflection)
// Uses LittleEndian to match SyncOpsCodec style

func (c *MemberMsgCodec) encodeString(s string) []byte {
	b := []byte(s)
	// Use fixed buffer pool for length encoding
	lenBuf := fixed8BufPool.Get().([]byte)[:2]
	binary.LittleEndian.PutUint16(lenBuf, uint16(len(b)))
	result := append(lenBuf, b...)
	fixed8BufPool.Put(lenBuf[:8]) // Return full buffer
	return result
}

func (c *MemberMsgCodec) decodeString(data []byte, offset int) (string, int) {
	if offset+2 > len(data) {
		// Silently return error - data may be truncated (e.g., node shutdown during transmission)
		return "", 0
	}
	strLen := int(binary.LittleEndian.Uint16(data[offset:]))
	offset += 2
	if strLen < 0 {
		// Invalid length - data corruption or truncation
		return "", 0
	}
	if offset+strLen > len(data) {
		// Data truncated - likely due to node shutdown during transmission
		// Silently return error instead of logging warning to reduce noise
		return "", 0
	}
	if strLen == 0 {
		return "", offset
	}
	// Additional safety check for extremely large strings (likely corruption)
	if strLen > len(data) || strLen > 65535 {
		return "", 0
	}
	result := string(data[offset : offset+strLen])
	if len(result) != strLen {
		// Length mismatch - data corruption
		return "", 0
	}
	return result, offset + strLen
}

func (c *MemberMsgCodec) encodePingMsg(msg *pingMsg) []byte {
	buf := smallBufPool.Get().([]byte)
	buf = buf[:0] // Reset length

	buf = append(buf, c.encodeString(msg.From)...)
	buf = append(buf, c.encodeString(msg.To)...)

	// Use fixed buffer pool for incarnation
	incBuf := fixed8BufPool.Get().([]byte)
	binary.LittleEndian.PutUint64(incBuf, uint64(msg.Incarnation))
	buf = append(buf, incBuf...)
	fixed8BufPool.Put(incBuf)

	// Return copy, return buffer to pool
	result := make([]byte, len(buf))
	copy(result, buf)
	smallBufPool.Put(buf[:0])
	return result
}

func (c *MemberMsgCodec) decodePingMsg(data []byte) *pingMsg {
	if len(data) < 2 {
		return nil
	}
	msg := &pingMsg{}
	offset := 0
	msg.From, offset = c.decodeString(data, offset)
	if offset == 0 {
		return nil
	}
	msg.To, offset = c.decodeString(data, offset)
	if offset == 0 || offset+8 > len(data) {
		return nil
	}
	msg.Incarnation = int64(binary.LittleEndian.Uint64(data[offset:]))
	return msg
}

func (c *MemberMsgCodec) encodeAckMsg(msg *ackMsg) []byte {
	buf := smallBufPool.Get().([]byte)
	buf = buf[:0] // Reset length

	buf = append(buf, c.encodeString(msg.From)...)
	buf = append(buf, c.encodeString(msg.To)...)

	// Use fixed buffer pool for incarnation
	incBuf := fixed8BufPool.Get().([]byte)
	binary.LittleEndian.PutUint64(incBuf, uint64(msg.Incarnation))
	buf = append(buf, incBuf...)
	fixed8BufPool.Put(incBuf)

	// Return copy, return buffer to pool
	result := make([]byte, len(buf))
	copy(result, buf)
	smallBufPool.Put(buf[:0])
	return result
}

func (c *MemberMsgCodec) decodeAckMsg(data []byte) *ackMsg {
	if len(data) < 2 {
		return nil
	}
	msg := &ackMsg{}
	offset := 0
	msg.From, offset = c.decodeString(data, offset)
	if offset == 0 {
		return nil
	}
	msg.To, offset = c.decodeString(data, offset)
	if offset == 0 || offset+8 > len(data) {
		return nil
	}
	msg.Incarnation = int64(binary.LittleEndian.Uint64(data[offset:]))
	return msg
}

func (c *MemberMsgCodec) encodeConnectMsg(msg *connectMsg) []byte {
	buf := mediumBufPool.Get().([]byte)
	buf = buf[:0] // Reset length

	buf = append(buf, c.encodeString(msg.NodeID)...)
	buf = append(buf, c.encodeString(msg.Address)...)

	// Use fixed buffer pool for incarnation
	incBuf := fixed8BufPool.Get().([]byte)
	binary.LittleEndian.PutUint64(incBuf, uint64(msg.Incarnation))
	buf = append(buf, incBuf...)
	fixed8BufPool.Put(incBuf)

	// Return copy, return buffer to pool
	result := make([]byte, len(buf))
	copy(result, buf)
	mediumBufPool.Put(buf[:0])
	return result
}

func (c *MemberMsgCodec) decodeConnectMsg(data []byte) *connectMsg {
	if len(data) < 2 {
		return nil
	}
	msg := &connectMsg{}
	offset := 0
	end := 30
	if end > len(data) {
		end = len(data)
	}
	msg.NodeID, offset = c.decodeString(data, offset)
	if offset == 0 {
		logging.Warn("Failed to decode NodeID from connect message", "dataLen", len(data))
		return nil
	}
	if len(msg.NodeID) == 0 {
		logging.Warn("Empty NodeID decoded from connect message", "offset", offset, "dataLen", len(data))
		return nil
	}
	expectedOffset := 2 + len(msg.NodeID)
	if offset != expectedOffset {
		end2 := offset + 10
		if end2 > len(data) {
			end2 = len(data)
		}
		logging.Warn("NodeID offset mismatch in connect message", "nodeID", msg.NodeID, "expectedOffset", expectedOffset, "actualOffset", offset)
		return nil
	}
	addressOffset := offset
	msg.Address, offset = c.decodeString(data, offset)
	if offset == 0 {
		end := addressOffset + 20
		if end > len(data) {
			end = len(data)
		}
		logging.Warn("Failed to decode address from connect message", "nodeID", msg.NodeID, "addressOffset", addressOffset)
		return nil
	}
	if len(msg.Address) < 3 || msg.Address[0] == 0 {
		end := addressOffset + 20
		if end > len(data) {
			end = len(data)
		}
		endBefore := addressOffset
		if endBefore > len(data) {
			endBefore = len(data)
		}
		logging.Warn("Invalid address in connect message", "nodeID", msg.NodeID, "address", fmt.Sprintf("%q", msg.Address), "addressLen", len(msg.Address))
		return nil
	}
	if offset+8 > len(data) {
		return nil
	}
	msg.Incarnation = int64(binary.LittleEndian.Uint64(data[offset:]))
	return msg
}

func (c *MemberMsgCodec) encodeLeaveMsg(msg *leaveMsg) []byte {
	buf := smallBufPool.Get().([]byte)
	buf = buf[:0] // Reset length

	buf = append(buf, c.encodeString(msg.NodeID)...)

	// Use fixed buffer pool for incarnation
	incBuf := fixed8BufPool.Get().([]byte)
	binary.LittleEndian.PutUint64(incBuf, uint64(msg.Incarnation))
	buf = append(buf, incBuf...)
	fixed8BufPool.Put(incBuf)

	// Return copy, return buffer to pool
	result := make([]byte, len(buf))
	copy(result, buf)
	smallBufPool.Put(buf[:0])
	return result
}

func (c *MemberMsgCodec) decodeLeaveMsg(data []byte) *leaveMsg {
	if len(data) < 2 {
		return nil
	}
	msg := &leaveMsg{}
	offset := 0
	msg.NodeID, offset = c.decodeString(data, offset)
	if offset == 0 || offset+8 > len(data) {
		return nil
	}
	msg.Incarnation = int64(binary.LittleEndian.Uint64(data[offset:]))
	return msg
}

func (c *MemberMsgCodec) encodeIndirectProbeMsg(msg *indirectProbeMsg) []byte {
	buf := smallBufPool.Get().([]byte)
	buf = buf[:0] // Reset length

	buf = append(buf, c.encodeString(msg.From)...)
	buf = append(buf, c.encodeString(msg.Target)...)

	// Use fixed buffer pool for incarnation
	incBuf := fixed8BufPool.Get().([]byte)
	binary.LittleEndian.PutUint64(incBuf, uint64(msg.Incarnation))
	buf = append(buf, incBuf...)
	fixed8BufPool.Put(incBuf)

	// Return copy, return buffer to pool
	result := make([]byte, len(buf))
	copy(result, buf)
	smallBufPool.Put(buf[:0])
	return result
}

func (c *MemberMsgCodec) decodeIndirectProbeMsg(data []byte) *indirectProbeMsg {
	if len(data) < 2 {
		return nil
	}
	msg := &indirectProbeMsg{}
	offset := 0
	msg.From, offset = c.decodeString(data, offset)
	if offset == 0 {
		return nil
	}
	msg.Target, offset = c.decodeString(data, offset)
	if offset == 0 || offset+8 > len(data) {
		return nil
	}
	msg.Incarnation = int64(binary.LittleEndian.Uint64(data[offset:]))
	return msg
}

func (c *MemberMsgCodec) encodeNodeInfo(info *NodeInfo) []byte {
	buf := mediumBufPool.Get().([]byte)
	buf = buf[:0] // Reset length

	buf = append(buf, c.encodeString(info.NodeID)...)
	buf = append(buf, c.encodeString(info.Address)...)

	// Use fixed buffer for state (4 bytes from 8-byte pool)
	stateBuf := fixed8BufPool.Get().([]byte)[:4]
	binary.LittleEndian.PutUint32(stateBuf, uint32(info.State))
	buf = append(buf, stateBuf...)
	fixed8BufPool.Put(stateBuf[:8])

	// Use fixed buffer pool for incarnation
	incBuf := fixed8BufPool.Get().([]byte)
	binary.LittleEndian.PutUint64(incBuf, uint64(info.Incarnation))
	buf = append(buf, incBuf...)
	fixed8BufPool.Put(incBuf)

	// Use fixed buffer pool for timestamp
	timeBuf := fixed8BufPool.Get().([]byte)
	binary.LittleEndian.PutUint64(timeBuf, uint64(info.LastActive.UnixNano()))
	buf = append(buf, timeBuf...)
	fixed8BufPool.Put(timeBuf)

	// Return copy, return buffer to pool
	result := make([]byte, len(buf))
	copy(result, buf)
	mediumBufPool.Put(buf[:0])
	return result
}

func (c *MemberMsgCodec) decodeNodeInfo(data []byte, offset int) (*NodeInfo, int) {
	if offset+2 > len(data) {
		return nil, offset
	}
	info := &NodeInfo{}
	info.NodeID, offset = c.decodeString(data, offset)
	if offset == 0 {
		return nil, offset
	}
	info.Address, offset = c.decodeString(data, offset)
	if offset == 0 {
		logging.Warn("Failed to decode address from node info", "nodeID", info.NodeID)
		return nil, offset
	}
	if info.Address == "" {
		logging.Warn("Empty address decoded from node info", "nodeID", info.NodeID)
	} else if len(info.Address) < 3 || info.Address[0] == 0 {
		logging.Warn("Invalid address decoded from node info", "nodeID", info.NodeID, "address", fmt.Sprintf("%q", info.Address))
	}
	if offset+4 > len(data) {
		return nil, offset
	}
	info.State = NodeState(binary.LittleEndian.Uint32(data[offset:]))
	offset += 4
	if offset+8 > len(data) {
		return nil, offset
	}
	info.Incarnation = int64(binary.LittleEndian.Uint64(data[offset:]))
	offset += 8
	if offset+8 > len(data) {
		return nil, offset
	}
	unixNano := int64(binary.LittleEndian.Uint64(data[offset:]))
	info.LastActive = time.Unix(0, unixNano)
	offset += 8
	return info, offset
}

func (c *MemberMsgCodec) encodeClusterSyncMsg(msg *clusterSyncMsg) ([]byte, error) {
	// Estimate size for better allocation
	estimatedSize := 256 + len(msg.Members)*128
	buf := make([]byte, 0, estimatedSize)

	buf = append(buf, c.encodeString(msg.From)...)

	// Use fixed buffer for member count (4 bytes from 8-byte pool)
	memberCountBuf := fixed8BufPool.Get().([]byte)[:4]
	binary.LittleEndian.PutUint32(memberCountBuf, uint32(len(msg.Members)))
	buf = append(buf, memberCountBuf...)
	fixed8BufPool.Put(memberCountBuf[:8])

	for i := range msg.Members {
		buf = append(buf, c.encodeNodeInfo(&msg.Members[i])...)
	}
	return buf, nil
}

func (c *MemberMsgCodec) decodeClusterSyncMsg(data []byte) *clusterSyncMsg {
	if len(data) < 2 {
		return nil
	}
	msg := &clusterSyncMsg{}
	offset := 0
	msg.From, offset = c.decodeString(data, offset)
	if offset == 0 {
		logging.Warn("Failed to decode sender from cluster sync message")
		return nil
	}
	if offset+4 > len(data) {
		logging.Warn("Insufficient data for member count in cluster sync message", "offset", offset, "dataLen", len(data))
		return nil
	}
	memberCount := int(binary.LittleEndian.Uint32(data[offset:]))
	offset += 4
	if memberCount < 0 || memberCount > 1000 {
		logging.Warn("Invalid member count in cluster sync message", "count", memberCount)
		return nil
	}
	msg.Members = make([]NodeInfo, 0, memberCount)
	for i := 0; i < memberCount; i++ {
		info, newOffset := c.decodeNodeInfo(data, offset)
		if info == nil {
			logging.Warn("Failed to decode member in cluster sync message", "index", i)
			return nil
		}
		if info.Address == "" || len(info.Address) < 3 || info.Address[0] == 0 {
			logging.Warn("Member has invalid address in cluster sync message", "index", i, "nodeID", info.NodeID, "address", fmt.Sprintf("%q", info.Address))
			return nil
		}
		msg.Members = append(msg.Members, *info)
		offset = newOffset
	}
	return msg
}

// Global codec instance (thread-safe, stateless)
var memberMsgCodec = &MemberMsgCodec{}

// EncodeMemberMsg encodes member message (convenience function)
func EncodeMemberMsg(msg interface{}) ([]byte, error) {
	return memberMsgCodec.EncodeMemberMsg(msg)
}

// DecodeMemberMsg decodes member message (convenience function)
func DecodeMemberMsg(data []byte, msgType uint8) interface{} {
	return memberMsgCodec.DecodeMemberMsg(data, msgType)
}
