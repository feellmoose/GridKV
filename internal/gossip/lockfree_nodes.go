package gossip

import (
	"sync/atomic"
)

type LockFreeNodeMap struct {
	data atomic.Value
}

func NewLockFreeNodeMap() *LockFreeNodeMap {
	nm := &LockFreeNodeMap{}
	nm.data.Store(&map[string]*NodeInfo{})
	return nm
}

func (nm *LockFreeNodeMap) Get(id string) (*NodeInfo, bool) {
	m := nm.data.Load().(*map[string]*NodeInfo)
	n, ok := (*m)[id]
	return n, ok
}

func (nm *LockFreeNodeMap) Update(id string, node *NodeInfo) {
	for {
		oldMap := nm.data.Load().(*map[string]*NodeInfo)
		newMap := make(map[string]*NodeInfo, len(*oldMap)+1)
		for k, v := range *oldMap {
			newMap[k] = v
		}
		newMap[id] = node
		if nm.data.CompareAndSwap(oldMap, &newMap) {
			return
		}
	}
}

func (nm *LockFreeNodeMap) Delete(id string) {
	for {
		oldMap := nm.data.Load().(*map[string]*NodeInfo)
		if _, exists := (*oldMap)[id]; !exists {
			return
		}
		newMap := make(map[string]*NodeInfo, len(*oldMap))
		for k, v := range *oldMap {
			if k != id {
				newMap[k] = v
			}
		}
		if nm.data.CompareAndSwap(oldMap, &newMap) {
			return
		}
	}
}

func (nm *LockFreeNodeMap) Range(fn func(string, *NodeInfo) bool) {
	m := nm.data.Load().(*map[string]*NodeInfo)
	for k, v := range *m {
		if !fn(k, v) {
			break
		}
	}
}

func (nm *LockFreeNodeMap) Len() int {
	m := nm.data.Load().(*map[string]*NodeInfo)
	return len(*m)
}

func (nm *LockFreeNodeMap) Snapshot() map[string]*NodeInfo {
	m := nm.data.Load().(*map[string]*NodeInfo)
	snapshot := make(map[string]*NodeInfo, len(*m))
	for k, v := range *m {
		snapshot[k] = v
	}
	return snapshot
}
