package actor

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/frankieli/actor_cluster/pkg/remote/pb"
)

const (
	statusInit int32 = iota + 1
	statusRunning
	statusStopping
	statusDead
)

// ResultSink receives asynchronous actor results.
type ResultSink interface {
	Emit(result *pb.RemoteResult)
}

// Handler is the user callback executed by one actor goroutine.
type Handler interface {
	Handle(env *pb.RemoteEnvelope) *pb.RemoteResult
}

// Envelope is the intrusive MPSC queue node.
// Msg holds a pointer to the pb.RemoteEnvelope; set to nil on recycle to release the reference.
type Envelope struct {
	Msg  *pb.RemoteEnvelope
	next atomic.Pointer[Envelope]
}

var envelopePool = &sync.Pool{
	New: func() any {
		return &Envelope{}
	},
}

// processMailboxPending tracks the total number of queued envelopes across all
// actor mailboxes inside the current node process. It is process-local, not
// per-actor and not cluster-global.
var processMailboxPending atomic.Uint64

// processHandleDurationNsCount / processHandleDurationNsSum 記錄每次
// handler.Handle() 的呼叫次數與累計耗時（nanoseconds）。
// 設計與 processMailboxPending 一致：process-local aggregate、atomic、無鎖。
var (
	processHandleDurationNsCount atomic.Uint64
	processHandleDurationNsSum   atomic.Uint64
)

// GetEnvelope exposes pooled envelopes for advanced zero-allocation adapters.
func GetEnvelope() *Envelope {
	return envelopePool.Get().(*Envelope)
}

// PutEnvelope clears and returns an envelope to the pool.
func PutEnvelope(env *Envelope) {
	if env == nil {
		return
	}
	env.Msg = nil
	env.next.Store(nil)
	envelopePool.Put(env)
}

// Actor serializes messages through a single callback goroutine.
type Actor struct {
	handler    Handler
	sink       ResultSink
	wake       chan struct{}
	shutdownFn func() // called in actor goroutine just before statusDead

	head   atomic.Pointer[Envelope]
	tail   atomic.Pointer[Envelope]
	status atomic.Int32

	_pad [5]uint64
}

// New constructs an actor with an initialized MPSC mailbox.
func New(handler Handler, sink ResultSink) *Actor {
	if handler == nil {
		panic("actor: nil Handler")
	}
	if sink == nil {
		panic("actor: nil ResultSink")
	}
	a := &Actor{
		handler: handler,
		sink:    sink,
		wake:    make(chan struct{}, 1),
	}
	dummy := &Envelope{}
	a.head.Store(dummy)
	a.tail.Store(dummy)
	a.status.Store(statusInit)
	return a
}

// Start runs the actor event loop in one goroutine.
func (a *Actor) Start(ctx context.Context) {
	if !a.status.CompareAndSwap(statusInit, statusRunning) {
		return
	}
	go a.loop(ctx)
}

// Send enqueues a message. It returns false when the actor is stopping or dead.
func (a *Actor) Send(env *pb.RemoteEnvelope) bool {
	st := a.status.Load()
	if st == statusStopping || st == statusDead {
		return false
	}
	qnode := GetEnvelope()
	qnode.Msg = env
	a.sendEnvelope(qnode)
	return true
}

// SendEnvelope enqueues a preallocated envelope. Ownership transfers to Actor on success.
func (a *Actor) SendEnvelope(env *Envelope) bool {
	if env == nil {
		return false
	}
	st := a.status.Load()
	if st == statusStopping || st == statusDead {
		return false
	}
	a.sendEnvelope(env)
	return true
}

func (a *Actor) sendEnvelope(env *Envelope) {
	env.next.Store(nil)
	processMailboxPending.Add(1)
	prev := a.tail.Swap(env)
	prev.next.Store(env)
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// SetShutdownFunc registers a function to be called in the actor goroutine
// just before it exits (after the last message is processed).
// Must be called before Start(); calling after Start() is a data race.
func (a *Actor) SetShutdownFunc(fn func()) {
	a.shutdownFn = fn
}

// Stop marks the actor as stopping and wakes the loop.
func (a *Actor) Stop() {
	for {
		st := a.status.Load()
		if st == statusStopping || st == statusDead {
			return
		}
		if a.status.CompareAndSwap(st, statusStopping) {
			select {
			case a.wake <- struct{}{}:
			default:
			}
			return
		}
	}
}

// IsAlive reports whether the actor accepts work.
func (a *Actor) IsAlive() bool {
	st := a.status.Load()
	return st == statusInit || st == statusRunning
}

// IsDead reports whether the actor goroutine has fully exited.
// This is distinct from IsAlive: after Stop(), IsAlive returns false (statusStopping),
// but the goroutine may still be executing the final Handle call.
// IsDead only returns true once the goroutine has reached statusDead and returned.
// Safe to use as a spin-wait guard before calling Drain().
func (a *Actor) IsDead() bool {
	return a.status.Load() == statusDead
}

// Drain returns all unprocessed envelopes that remain in the mailbox.
//
// MUST be called only after IsDead() returns true.
// Once the goroutine is dead, no concurrent dequeue() is happening,
// so walking head.next to tail is data-race-free.
func (a *Actor) Drain() []*pb.RemoteEnvelope {
	var envs []*pb.RemoteEnvelope
	for {
		head := a.head.Load()
		next := head.next.Load()
		if next == nil {
			break
		}
		envs = append(envs, next.Msg)
		next.Msg = nil // clear reference before this node becomes the new dummy
		a.head.Store(next)
		processMailboxPending.Add(^uint64(0))
		PutEnvelope(head)
	}
	return envs
}

func (a *Actor) dequeue() *Envelope {
	head := a.head.Load()
	next := head.next.Load()
	if next == nil {
		if head != a.tail.Load() {
			runtime.Gosched()
		}
		return nil
	}
	a.head.Store(next)
	processMailboxPending.Add(^uint64(0))
	PutEnvelope(head)
	return next
}

func (a *Actor) loop(ctx context.Context) {
	for {
		qnode := a.dequeue()
		if qnode == nil {
			if a.status.Load() == statusStopping {
				if a.shutdownFn != nil {
					a.shutdownFn()
				}
				a.status.Store(statusDead)
				return
			}
			select {
			case <-ctx.Done():
				a.status.Store(statusDead)
				return
			case <-a.wake:
			}
			continue
		}

		env := qnode.Msg
		handleStart := time.Now()
		result := a.handler.Handle(env)
		processHandleDurationNsSum.Add(uint64(time.Since(handleStart)))
		processHandleDurationNsCount.Add(1)
		if result != nil {
			completeResult(result, env)
			a.sink.Emit(result)
		}
		qnode.Msg = nil // release reference; qnode is now the dummy head
	}
}

// completeResult fills in envelope metadata from the pb.RemoteEnvelope when the handler
// leaves them zero, so handlers only need to set Success/ErrorCode/Payload.
func completeResult(result *pb.RemoteResult, env *pb.RemoteEnvelope) {
	if result.TenantId == 0 {
		result.TenantId = env.TenantId
	}
	if result.Uid == 0 {
		result.Uid = env.Uid
	}
	if result.RequestId == 0 {
		result.RequestId = env.RequestId
	}
	if result.TxId == "" {
		result.TxId = env.TxId
	}
	if result.OpCode == 0 {
		result.OpCode = env.OpCode
	}
}

func ProcessMailboxPending() uint64 {
	return processMailboxPending.Load()
}

// ProcessHandleDurationNsSum 回傳自 process 啟動以來所有 handler.Handle()
// 的累計耗時（nanoseconds）。
func ProcessHandleDurationNsSum() uint64 {
	return processHandleDurationNsSum.Load()
}

// ProcessHandleDurationNsCount 回傳自 process 啟動以來的 Handle() 總呼叫次數。
func ProcessHandleDurationNsCount() uint64 {
	return processHandleDurationNsCount.Load()
}
