package gossip

import (
	"testing"
	"time"
)

func TestBinarySerializationRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		message *GossipMessage
	}{
		{
			name: "ConnectPayload",
			message: &GossipMessage{
				Type:   MessageTypeConnect,
				Sender: "node1",
				ConnectPayload: &ConnectPayload{
					NodeId:    "node1",
					Address:   "127.0.0.1:8001",
					Version:   1,
					Hlc:       "node1:1234567890:1",
					PublicKey: []byte("public-key-32-bytes-long-example"),
				},
			},
		},
		{
			name: "ClusterSyncPayload",
			message: &GossipMessage{
				Type:   MessageTypeClusterSync,
				Sender: "node1",
				ClusterSyncPayload: &ClusterSyncPayload{
					Nodes: []*NodeInfo{
						{
							NodeId:       "node1",
							Address:      "127.0.0.1:8001",
							LastActiveTs: time.Now(),
							State:        NodeStateAlive,
							Version:      1,
						},
						{
							NodeId:       "node2",
							Address:      "127.0.0.1:8002",
							LastActiveTs: time.Now(),
							State:        NodeStateAlive,
							Version:      2,
						},
					},
				},
			},
		},
		{
			name: "ProbeRequestPayload",
			message: &GossipMessage{
				Type:   MessageTypeProbeRequest,
				Sender: "node1",
				ProbeRequestPayload: &ProbePayload{
					TargetNodeId: "node2",
					RequesterId:  "node1",
				},
			},
		},
		{
			name: "ProbeResponsePayload",
			message: &GossipMessage{
				Type:   MessageTypeProbeResponse,
				Sender: "node2",
				ProbeResponsePayload: &ProbeResponsePayload{
					TargetNodeId: "node1",
					Alive:        true,
				},
			},
		},
		{
			name: "CacheSyncPayload",
			message: &GossipMessage{
				Type:   MessageTypeCacheSync,
				Sender: "node1",
				CacheSyncPayload: &SyncMessage{
					IncrementalSync: &IncrementalSyncPayload{
						Operations: []*CacheSyncOperation{
							{
								Key:           "key1",
								ClientVersion: 1,
								Type:          OpSet,
								SetData: &StoredItem{
									ExpireAt: uint64(time.Now().Unix() + 3600),
									Value:    []byte("value1"),
								},
							},
							{
								Key:           "key2",
								ClientVersion: 2,
								Type:          OpDelete,
							},
						},
						PayloadId: "payload-1",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binary := convertGossipMessageToBinary(tt.message)
			if binary == nil {
				t.Fatalf("Failed to convert message to binary")
			}

			data := binary.Marshal()
			if len(data) == 0 {
				t.Fatalf("Serialized data is empty")
			}

			binaryMsg, err := UnmarshalBinaryMessage(data)
			if err != nil {
				t.Fatalf("Failed to unmarshal binary message: %v", err)
			}

			decodedMsg := convertBinaryToGossipMessage(binaryMsg)
			if decodedMsg == nil {
				t.Fatalf("Failed to convert binary message back to GossipMessage")
			}

			if decodedMsg.Type != tt.message.Type {
				t.Errorf("Type mismatch: got %v, want %v", decodedMsg.Type, tt.message.Type)
			}
			if decodedMsg.Sender != tt.message.Sender {
				t.Errorf("Sender mismatch: got %s, want %s", decodedMsg.Sender, tt.message.Sender)
			}

			switch tt.message.Type {
			case MessageTypeConnect:
				if decodedMsg.ConnectPayload == nil {
					t.Fatal("ConnectPayload is nil")
				}
				if decodedMsg.ConnectPayload.NodeId != tt.message.ConnectPayload.NodeId {
					t.Errorf("NodeId mismatch: got %s, want %s", decodedMsg.ConnectPayload.NodeId, tt.message.ConnectPayload.NodeId)
				}
				if decodedMsg.ConnectPayload.Address != tt.message.ConnectPayload.Address {
					t.Errorf("Address mismatch: got %s, want %s", decodedMsg.ConnectPayload.Address, tt.message.ConnectPayload.Address)
				}
				if decodedMsg.ConnectPayload.Version != tt.message.ConnectPayload.Version {
					t.Errorf("Version mismatch: got %d, want %d", decodedMsg.ConnectPayload.Version, tt.message.ConnectPayload.Version)
				}

			case MessageTypeClusterSync:
				if decodedMsg.ClusterSyncPayload == nil {
					t.Fatal("ClusterSyncPayload is nil")
				}
				if len(decodedMsg.ClusterSyncPayload.Nodes) != len(tt.message.ClusterSyncPayload.Nodes) {
					t.Errorf("Nodes count mismatch: got %d, want %d", len(decodedMsg.ClusterSyncPayload.Nodes), len(tt.message.ClusterSyncPayload.Nodes))
				}
				for i, node := range decodedMsg.ClusterSyncPayload.Nodes {
					if node.NodeId != tt.message.ClusterSyncPayload.Nodes[i].NodeId {
						t.Errorf("Node[%d].NodeId mismatch: got %s, want %s", i, node.NodeId, tt.message.ClusterSyncPayload.Nodes[i].NodeId)
					}
					if node.Address != tt.message.ClusterSyncPayload.Nodes[i].Address {
						t.Errorf("Node[%d].Address mismatch: got %s, want %s", i, node.Address, tt.message.ClusterSyncPayload.Nodes[i].Address)
					}
					if node.State != tt.message.ClusterSyncPayload.Nodes[i].State {
						t.Errorf("Node[%d].State mismatch: got %v, want %v", i, node.State, tt.message.ClusterSyncPayload.Nodes[i].State)
					}
				}

			case MessageTypeProbeRequest:
				if decodedMsg.ProbeRequestPayload == nil {
					t.Fatal("ProbeRequestPayload is nil")
				}
				if decodedMsg.ProbeRequestPayload.TargetNodeId != tt.message.ProbeRequestPayload.TargetNodeId {
					t.Errorf("TargetNodeId mismatch: got %s, want %s", decodedMsg.ProbeRequestPayload.TargetNodeId, tt.message.ProbeRequestPayload.TargetNodeId)
				}

			case MessageTypeProbeResponse:
				if decodedMsg.ProbeResponsePayload == nil {
					t.Fatal("ProbeResponsePayload is nil")
				}
				if decodedMsg.ProbeResponsePayload.Alive != tt.message.ProbeResponsePayload.Alive {
					t.Errorf("Alive mismatch: got %v, want %v", decodedMsg.ProbeResponsePayload.Alive, tt.message.ProbeResponsePayload.Alive)
				}

			case MessageTypeCacheSync:
				if decodedMsg.CacheSyncPayload == nil {
					t.Fatal("CacheSyncPayload is nil")
				}
				if decodedMsg.CacheSyncPayload.IncrementalSync == nil {
					t.Fatal("IncrementalSync is nil")
				}
				if len(decodedMsg.CacheSyncPayload.IncrementalSync.Operations) != len(tt.message.CacheSyncPayload.IncrementalSync.Operations) {
					t.Errorf("Operations count mismatch: got %d, want %d", len(decodedMsg.CacheSyncPayload.IncrementalSync.Operations), len(tt.message.CacheSyncPayload.IncrementalSync.Operations))
				}
				for i, op := range decodedMsg.CacheSyncPayload.IncrementalSync.Operations {
					if op.Key != tt.message.CacheSyncPayload.IncrementalSync.Operations[i].Key {
						t.Errorf("Operation[%d].Key mismatch: got %s, want %s", i, op.Key, tt.message.CacheSyncPayload.IncrementalSync.Operations[i].Key)
					}
					if op.Type != tt.message.CacheSyncPayload.IncrementalSync.Operations[i].Type {
						t.Errorf("Operation[%d].Type mismatch: got %v, want %v", i, op.Type, tt.message.CacheSyncPayload.IncrementalSync.Operations[i].Type)
					}
					if op.Type == OpSet {
						if op.SetData == nil {
							t.Errorf("Operation[%d].SetData is nil for SET operation", i)
						} else if string(op.SetData.Value) != string(tt.message.CacheSyncPayload.IncrementalSync.Operations[i].SetData.Value) {
							t.Errorf("Operation[%d].SetData.Value mismatch: got %s, want %s", i, string(op.SetData.Value), string(tt.message.CacheSyncPayload.IncrementalSync.Operations[i].SetData.Value))
						}
					}
				}
			}

			PutBinaryMessage(binaryMsg)
		})
	}
}

func TestBinaryOperationsEncoding(t *testing.T) {
	ops := make([]*CacheSyncOperation, 1000)
	for i := range ops {
		ops[i] = &CacheSyncOperation{
			Key:           "test-key-" + string(rune(i)),
			ClientVersion: int64(i),
			Type:          OperationType_OP_SET,
			SetData: &StoredItem{
				Value:    make([]byte, 1024),
				ExpireAt: uint64(time.Now().Unix()),
			},
		}
	}

	start := time.Now()
	encoded := EncodeOperations(ops)
	encodeTime := time.Since(start)

	start = time.Now()
	decoded, err := DecodeOperations(encoded)
	decodeTime := time.Since(start)

	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(decoded) != len(ops) {
		t.Fatalf("Decoded count mismatch: got %d, want %d", len(decoded), len(ops))
	}

	t.Logf("Encode: %v, Decode: %v, Size: %d bytes", encodeTime, decodeTime, len(encoded))
}

func TestBinarySerializationPerformance(t *testing.T) {
	ops := make([]*CacheSyncOperation, 1000)
	for i := range ops {
		ops[i] = &CacheSyncOperation{
			Key:           "key-" + string(rune(i)),
			ClientVersion: int64(i),
			Type:          OpSet,
			SetData: &StoredItem{
				ExpireAt: uint64(time.Now().Unix() + 3600),
				Value:    make([]byte, 256),
			},
		}
	}

	msg := &GossipMessage{
		Type:   MessageTypeCacheSync,
		Sender: "node1",
		CacheSyncPayload: &SyncMessage{
			IncrementalSync: &IncrementalSyncPayload{
				Operations: ops,
				PayloadId:  "test-payload",
			},
		},
	}

	t.Run("Serialize", func(t *testing.T) {
		start := time.Now()
		for i := 0; i < 100; i++ {
			binary := convertGossipMessageToBinary(msg)
			if binary == nil {
				t.Fatal("Failed to convert")
			}
			data := binary.Marshal()
			PutBinaryMessage(binary)
			if len(data) == 0 {
				t.Fatal("Empty data")
			}
		}
		elapsed := time.Since(start)
		t.Logf("Serialized 100 messages in %v (avg: %v per message)", elapsed, elapsed/100)
	})

	t.Run("Deserialize", func(t *testing.T) {
		binary := convertGossipMessageToBinary(msg)
		data := binary.Marshal()
		PutBinaryMessage(binary)

		start := time.Now()
		for i := 0; i < 100; i++ {
			binaryMsg, err := UnmarshalBinaryMessage(data)
			if err != nil {
				t.Fatalf("Failed to unmarshal: %v", err)
			}
			decoded := convertBinaryToGossipMessage(binaryMsg)
			if decoded == nil {
				t.Fatal("Failed to decode")
			}
			PutBinaryMessage(binaryMsg)
		}
		elapsed := time.Since(start)
		t.Logf("Deserialized 100 messages in %v (avg: %v per message)", elapsed, elapsed/100)
	})

	t.Run("RoundTrip", func(t *testing.T) {
		start := time.Now()
		for i := 0; i < 100; i++ {
			binary := convertGossipMessageToBinary(msg)
			data := binary.Marshal()
			PutBinaryMessage(binary)

			binaryMsg, err := UnmarshalBinaryMessage(data)
			if err != nil {
				t.Fatalf("Failed to unmarshal: %v", err)
			}
			decoded := convertBinaryToGossipMessage(binaryMsg)
			if decoded == nil {
				t.Fatal("Failed to decode")
			}
			PutBinaryMessage(binaryMsg)
		}
		elapsed := time.Since(start)
		t.Logf("Round trip 100 messages in %v (avg: %v per message)", elapsed, elapsed/100)
	})
}

func TestBinaryMessageSize(t *testing.T) {
	tests := []struct {
		name    string
		message *GossipMessage
		wantMin int
	}{
		{
			name: "Small ConnectPayload",
			message: &GossipMessage{
				Type:   MessageTypeConnect,
				Sender: "node1",
				ConnectPayload: &ConnectPayload{
					NodeId:    "node1",
					Address:   "127.0.0.1:8001",
					Version:   1,
					Hlc:       "node1:1234567890:1",
					PublicKey: []byte("key"),
				},
			},
			wantMin: 50,
		},
		{
			name: "Large CacheSyncPayload",
			message: &GossipMessage{
				Type:   MessageTypeCacheSync,
				Sender: "node1",
				CacheSyncPayload: &SyncMessage{
					IncrementalSync: &IncrementalSyncPayload{
						Operations: []*CacheSyncOperation{
							{
								Key:           "very-long-key-name-that-takes-more-space",
								ClientVersion: 1,
								Type:          OpSet,
								SetData: &StoredItem{
									ExpireAt: uint64(time.Now().Unix() + 3600),
									Value:    make([]byte, 1024),
								},
							},
						},
					},
				},
			},
			wantMin: 1100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binary := convertGossipMessageToBinary(tt.message)
			if binary == nil {
				t.Fatal("Failed to convert")
			}
			data := binary.Marshal()
			PutBinaryMessage(binary)

			if len(data) < tt.wantMin {
				t.Errorf("Data size too small: got %d, want at least %d", len(data), tt.wantMin)
			}
			t.Logf("Message size: %d bytes", len(data))
		})
	}
}

func TestBinarySerializationEdgeCases(t *testing.T) {
	t.Run("EmptyMessage", func(t *testing.T) {
		msg := &GossipMessage{
			Type:   MessageTypeUnknown,
			Sender: "",
		}
		binary := convertGossipMessageToBinary(msg)
		if binary != nil {
			t.Error("Expected nil for unknown message type")
		}
	})

	t.Run("EmptyPayload", func(t *testing.T) {
		msg := &GossipMessage{
			Type:   MessageTypeConnect,
			Sender: "node1",
		}
		binary := convertGossipMessageToBinary(msg)
		if binary == nil {
			t.Fatal("Should handle nil payload")
		}
		data := binary.Marshal()
		decoded, err := UnmarshalBinaryMessage(data)
		if err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}
		if decoded.Type != BinaryMsgTypeConnect {
			t.Errorf("Type mismatch: got %d, want %d", decoded.Type, BinaryMsgTypeConnect)
		}
		PutBinaryMessage(binary)
		PutBinaryMessage(decoded)
	})

	t.Run("VeryLongSender", func(t *testing.T) {
		longSender := make([]byte, 20)
		for i := range longSender {
			longSender[i] = byte('a' + i%26)
		}
		msg := &GossipMessage{
			Type:   MessageTypeConnect,
			Sender: string(longSender),
			ConnectPayload: &ConnectPayload{
				NodeId:  "node1",
				Address: "127.0.0.1:8001",
				Version: 1,
			},
		}
		binary := convertGossipMessageToBinary(msg)
		if binary == nil {
			t.Fatal("Failed to convert")
		}
		if len(binary.Sender) != 16 {
			t.Errorf("Sender length mismatch: got %d, want 16", len(binary.Sender))
		}
		PutBinaryMessage(binary)
	})
}

func TestLockFreeNodeMap(t *testing.T) {
	nm := NewLockFreeNodeMap()

	node1 := &NodeInfo{
		NodeId:  "node1",
		Address: "127.0.0.1:8001",
		State:   NodeState_NODE_STATE_ALIVE,
		Version: 1,
	}

	nm.Update("node1", node1)

	n, ok := nm.Get("node1")
	if !ok {
		t.Fatal("Node not found")
	}

	if n.NodeId != "node1" {
		t.Fatalf("Node ID mismatch: got %s, want node1", n.NodeId)
	}

	nm.Delete("node1")

	_, ok = nm.Get("node1")
	if ok {
		t.Fatal("Node should be deleted")
	}
}

func TestHashRingCache(t *testing.T) {
	ring := NewConsistentHash(150, nil)
	ring.Add("node1")
	ring.Add("node2")
	ring.Add("node3")

	cache := NewHashRingCache(ring, 1*time.Second, 1000)

	key := "test-key"
	replicas1 := cache.GetN(key, 2)
	replicas2 := cache.GetN(key, 2)

	if len(replicas1) != 2 || len(replicas2) != 2 {
		t.Fatalf("Replica count mismatch")
	}

	if replicas1[0] != replicas2[0] || replicas1[1] != replicas2[1] {
		t.Fatal("Cache should return same results")
	}
}

func BenchmarkBinaryEncode(b *testing.B) {
	ops := make([]*CacheSyncOperation, 100)
	for i := range ops {
		ops[i] = &CacheSyncOperation{
			Key:           "key-" + string(rune(i)),
			ClientVersion: int64(i),
			Type:          OperationType_OP_SET,
			SetData: &StoredItem{
				Value:    make([]byte, 256),
				ExpireAt: uint64(time.Now().Unix()),
			},
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EncodeOperations(ops)
	}
}

func BenchmarkBinaryDecode(b *testing.B) {
	ops := make([]*CacheSyncOperation, 100)
	for i := range ops {
		ops[i] = &CacheSyncOperation{
			Key:           "key-" + string(rune(i)),
			ClientVersion: int64(i),
			Type:          OperationType_OP_SET,
			SetData: &StoredItem{
				Value:    make([]byte, 256),
				ExpireAt: uint64(time.Now().Unix()),
			},
		}
	}
	encoded := EncodeOperations(ops)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = DecodeOperations(encoded)
	}
}

func BenchmarkLockFreeNodeMapGet(b *testing.B) {
	nm := NewLockFreeNodeMap()
	for i := 0; i < 100; i++ {
		node := &NodeInfo{
			NodeId:  "node" + string(rune(i)),
			Address: "127.0.0.1:800" + string(rune(i)),
			State:   NodeState_NODE_STATE_ALIVE,
			Version: int64(i),
		}
		nm.Update("node"+string(rune(i)), node)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = nm.Get("node50")
	}
}
