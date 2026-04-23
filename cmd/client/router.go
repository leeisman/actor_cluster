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

type callbackEntry struct {
	startedAt time.Time
	respCh    chan *pb.RemoteResult
}

// ShardedCallbackMap 避免 global map lock contention
type ShardedCallbackMap struct {
	shards [256]struct {
		sync.RWMutex
		m map[uint64]callbackEntry
	}
}

func NewShardedCallbackMap() *ShardedCallbackMap {
	sm := &ShardedCallbackMap{}
	for i := 0; i < 256; i++ {
		sm.shards[i].m = make(map[uint64]callbackEntry, 1024)
	}
	return sm
}

func (sm *ShardedCallbackMap) Store(reqID uint64, entry callbackEntry) {
	shardIdx := reqID % 256
	s := &sm.shards[shardIdx]
	s.Lock()
	s.m[reqID] = entry
	s.Unlock()
}

func (sm *ShardedCallbackMap) LoadAndDelete(reqID uint64) (callbackEntry, bool) {
	shardIdx := reqID % 256
	s := &sm.shards[shardIdx]
	s.Lock()
	entry, ok := s.m[reqID]
	if ok {
		delete(s.m, reqID)
	}
	s.Unlock()
	return entry, ok
}

func (sm *ShardedCallbackMap) Delete(reqID uint64) {
	shardIdx := reqID % 256
	s := &sm.shards[shardIdx]
	s.Lock()
	delete(s.m, reqID)
	s.Unlock()
}

func (sm *ShardedCallbackMap) FailAll(router *Router, errCode string) {
	for i := 0; i < len(sm.shards); i++ {
		s := &sm.shards[i]
		s.Lock()
		for reqID, entry := range s.m {
			router.errCount.Add(1)
			router.RecordError(errCode)
			if entry.respCh != nil {
				entry.respCh <- &pb.RemoteResult{
					RequestId: reqID,
					Success:   false,
					ErrorCode: errCode,
					ErrorMsg:  errCode,
				}
			}
			delete(s.m, reqID)
		}
		s.Unlock()
	}
}

type Router struct {
	resolver   discovery.TopologyResolver
	batchSize  int
	flushDelay time.Duration

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
		resolver:   resolver,
		batchSize:  batchSize,
		flushDelay: flushDelay,
	}
}

func (r *Router) getStreamer(ip string) *NodeStreamer {
	if val, ok := r.streamers.Load(ip); ok {
		ns := val.(*NodeStreamer)
		if !ns.IsDead() {
			return ns
		}
		r.streamers.Delete(ip)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double check
	if val, ok := r.streamers.Load(ip); ok {
		ns := val.(*NodeStreamer)
		if !ns.IsDead() {
			return ns
		}
		r.streamers.Delete(ip)
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

func (r *Router) evictStreamer(ip string, ns *NodeStreamer) {
	if val, ok := r.streamers.Load(ip); ok && val == ns {
		r.streamers.Delete(ip)
	}
}

// EmulateGatewayFlow 模擬真實 Gateway 收發: Fire and forget 塞入佇列
//
// 路由目標由 TopologyResolver.GetNodeIP 提供；與 cmd/node 之 discovery 契約一致（見 docs/design/01_discovery_spec.md §14），與節點所有權一致。
func (r *Router) EmulateGatewayFlow(tenantID int32, uid int64, reqID uint64, env *pb.RemoteEnvelope) {
	r.dispatchEnvelope(tenantID, uid, reqID, env, callbackEntry{startedAt: time.Now()})
}

func (r *Router) ExecuteAndWait(env *pb.RemoteEnvelope, timeout time.Duration) *pb.RemoteResult {
	respCh := make(chan *pb.RemoteResult, 1)
	startedAt := time.Now()
	entry := callbackEntry{
		startedAt: startedAt,
		respCh:    respCh,
	}
	if immediate := r.dispatchEnvelope(env.TenantId, env.Uid, env.RequestId, env, entry); immediate != nil {
		return immediate
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-respCh:
		return res
	case <-timer.C:
		r.callbackTimeout(env.RequestId)
		return syntheticResult(env, remote.ErrDeadlineExceeded, "gateway wait timeout")
	}
}

func (r *Router) dispatchEnvelope(tenantID int32, uid int64, reqID uint64, env *pb.RemoteEnvelope, entry callbackEntry) *pb.RemoteResult {
	targetIP, err := r.resolver.GetNodeIP(tenantID, uid)
	if err != nil {
		r.errCount.Add(1)
		r.RecordError(remote.ErrDiscovery)
		return syntheticResult(env, remote.ErrDiscovery, err.Error())
	}

	for attempts := 0; attempts < 2; attempts++ {
		ns := r.getStreamer(targetIP)
		if ns == nil {
			r.errCount.Add(1)
			r.RecordError(remote.ErrConnection)
			return syntheticResult(env, remote.ErrConnection, remote.ErrConnection)
		}

		ns.callbackMap.Store(reqID, entry)
		select {
		case ns.envelopeCh <- env:
			return nil
		case <-ns.ctx.Done():
			ns.callbackMap.Delete(reqID)
		}
	}

	r.errCount.Add(1)
	r.RecordError(remote.ErrTransportClosed)
	return syntheticResult(env, remote.ErrTransportClosed, remote.ErrTransportClosed)
}

func (r *Router) callbackTimeout(reqID uint64) {
	r.errCount.Add(1)
	r.RecordError(remote.ErrDeadlineExceeded)
	r.streamers.Range(func(key, value interface{}) bool {
		value.(*NodeStreamer).callbackMap.Delete(reqID)
		return true
	})
}

func syntheticResult(env *pb.RemoteEnvelope, errCode, errMsg string) *pb.RemoteResult {
	return &pb.RemoteResult{
		TenantId:  env.TenantId,
		Uid:       env.Uid,
		TxId:      env.TxId,
		RequestId: env.RequestId,
		OpCode:    env.OpCode,
		Success:   false,
		ErrorCode: errCode,
		ErrorMsg:  errMsg,
	}
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

func (r *Router) ErrorBreakdownMap() map[string]uint64 {
	out := make(map[string]uint64)
	r.errBreakdown.Range(func(key, value interface{}) bool {
		out[key.(string)] = value.(*atomic.Uint64).Load()
		return true
	})
	return out
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
