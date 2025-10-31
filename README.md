# SentinelKV
SentinelKV is a lightweight, embedded Go SDK designed for building highly available, strongly consistent distributed session and state storage.

## Core Features:

Raft-Driven Consensus: Leverages the Raft protocol to ensure session data maintains Linearizability across all nodes in the cluster.

gRPC Networking Layer: Employs the high-performance gRPC framework for Raft message synchronization and cluster management between nodes, providing efficient and reliable network communication.

Session-Optimized FSM: The Finite State Machine (FSM) is optimized for session storage scenarios (e.g., in-memory cache FSM coupled with a persistent log) to achieve high-performance read and write operations.

Embedded Architecture: No external dependencies are required. Simply import the SDK and start the Raft instance within your application to quickly deploy a monolithic or distributed KV service.

**SentinelKV is the ideal solution for replacing traditional Redis/Memcached shared clusters to store critical state data such as token issuance records, user login sessions, and configuration flags.**
