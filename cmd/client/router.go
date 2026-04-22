package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frankieli/actor_cluster/pkg/discovery"
	"github.com/frankieli/actor_cluster/pkg/remote"
	"github.com/frankieli/actor_cluster/pkg/remote/pb"
)

// 回檔僅紀錄時間點用於 Latency 計算，已無 channel 等待

// ShardedCallbackMap 避免 global map lock contention
type ShardedCallbackMap struct {
	shards [256]struct {
		sync.RWMutex
		m map[uint64]time.Time
	}
}

func NewShardedCallbackMap() *ShardedCallbackMap {
	sm := &ShardedCallbackMap{}
	for i := 0; i < 256; i++ {
		sm.shards[i].m = make(map[uint64]time.Time, 1024)
	}
	return sm
}

func (sm *ShardedCallbackMap) Store(reqID uint64, startedAt time.Time) {
	shardIdx := reqID % 256
	s := &sm.shards[shardIdx]
	s.Lock()
	s.m[reqID] = startedAt
	s.Unlock()
}

func (sm *ShardedCallbackMap) LoadAndDelete(reqID uint64) (time.Time, bool) {
	shardIdx := reqID % 256
	s := &sm.shards[shardIdx]
	s.Lock()
	startedAt, ok := s.m[reqID]
	if ok {
		delete(s.m, reqID)
	}
	s.Unlock()
	return startedAt, ok
}

func (sm *ShardedCallbackMap) FailAll(router *Router, errCode string) {
	for i := 0; i < len(sm.shards); i++ {
		s := &sm.shards[i]
		s.Lock()
		for reqID := range s.m {
			router.errCount.Add(1)
			router.RecordError(errCode)
			delete(s.m, reqID)
		}
		s.Unlock()
	}
}

type Router struct {
	resolver    discovery.TopologyResolver
	batchSize   int
	flushDelay  time.Duration

	streamers sync.Map // map[string]*NodeStreamer
	mu        sync.Mutex

	// Metrics
	reqSent        atomic.Uint64
	respRecv       atomic.Uint64
	errCount       atomic.Uint64
	latencyNanos   atomic.Uint64
	latencySamples atomic.Uint64

	errBreakdown sync.Map // string -> *atomic.Uint64
}

func NewRouter(resolver discovery.TopologyResolver, batchSize int, flushDelay time.Duration) *Router {
	return &Router{
		resolver:    resolver,
		batchSize:   batchSize,
		flushDelay:  flushDelay,
	}
}

func (r *Router) getStreamer(ip string) *NodeStreamer {
	if val, ok := r.streamers.Load(ip); ok {
		return val.(*NodeStreamer)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double check
	if val, ok := r.streamers.Load(ip); ok {
		return val.(*NodeStreamer)
	}

	ns, err := NewNodeStreamer(
		ip,
		r.batchSize,
		r.flushDelay,
		r,
	)
	if err != nil {
		return nil
	}
	r.streamers.Store(ip, ns)
	return ns
}

// EmulateGatewayFlow 模擬真實 Gateway 收發: Fire and forget 塞入佇列
//
// 路由目標由 TopologyResolver.GetNodeIP 提供；與 cmd/node 之 discovery 契約一致（見 docs/design/01_discovery_spec.md §14），與節點所有權一致。
func (r *Router) EmulateGatewayFlow(tenantID int32, uid int64, reqID uint64, env *pb.RemoteEnvelope) {
	targetIP, err := r.resolver.GetNodeIP(tenantID, uid)
	if err != nil {
		r.errCount.Add(1)
		r.RecordError(remote.ErrDiscovery)
		return
	}

	ns := r.getStreamer(targetIP)
	if ns == nil {
		r.errCount.Add(1)
		r.RecordError(remote.ErrConnection)
		return
	}

	ns.callbackMap.Store(reqID, time.Now())
	ns.envelopeCh <- env
}

func (r *Router) RecordError(errCode string) {
	if errCode == "" {
		errCode = "UNKNOWN"
	}
	v, ok := r.errBreakdown.Load(errCode)
	if !ok {
		counter := &atomic.Uint64{}
		v, _ = r.errBreakdown.LoadOrStore(errCode, counter)
	}
	v.(*atomic.Uint64).Add(1)
}

func (r *Router) GetErrorBreakdown() string {
	var sb strings.Builder
	r.errBreakdown.Range(func(key, value interface{}) bool {
		sb.WriteString(fmt.Sprintf("%s:%d ", key.(string), value.(*atomic.Uint64).Load()))
		return true
	})
	res := sb.String()
	if res == "" {
		return "None"
	}
	return res
}

func (r *Router) RecordLatency(d time.Duration) {
	if d <= 0 {
		return
	}
	r.latencyNanos.Add(uint64(d))
	r.latencySamples.Add(1)
}

func (r *Router) AvgLatency() time.Duration {
	samples := r.latencySamples.Load()
	if samples == 0 {
		return 0
	}
	return time.Duration(r.latencyNanos.Load() / samples)
}

func (r *Router) StopAll() {
	r.streamers.Range(func(key, value interface{}) bool {
		ns := value.(*NodeStreamer)
		ns.StopAndWait()
		return true
	})
}
