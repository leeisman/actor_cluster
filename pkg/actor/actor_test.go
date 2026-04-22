package actor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/frankieli/actor_cluster/pkg/remote/pb"
)

func TestActor_SendInvokesHandlerAndEmitsResult(t *testing.T) {
	t.Parallel()

	sink := newRecordingSink(1)
	handler := handlerFunc(func(env *pb.RemoteEnvelope) *pb.RemoteResult {
		return &pb.RemoteResult{Success: true, Payload: env.Payload}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := New(handler, sink)
	a.Start(ctx)

	ok := a.Send(&pb.RemoteEnvelope{
		TenantId:  1,
		Uid:       100,
		RequestId: 7,
		TxId:      "tx-1",
		OpCode:    9,
		Payload:   []byte("ok"),
	})
	if !ok {
		t.Fatal("Send returned false")
	}

	res := sink.wait(t, time.Second)
	if !res.Success {
		t.Fatalf("expected success result, got %+v", res)
	}
	if res.RequestId != 7 || res.TxId != "tx-1" {
		t.Fatalf("result metadata not completed: %+v", res)
	}
	if string(res.Payload) != "ok" {
		t.Fatalf("expected payload ok, got %q", string(res.Payload))
	}
}

func TestActor_PreservesSingleProducerOrder(t *testing.T) {
	t.Parallel()

	const n = 32
	sink := newRecordingSink(n)
	handler := handlerFunc(func(env *pb.RemoteEnvelope) *pb.RemoteResult {
		return &pb.RemoteResult{Success: true, RequestId: env.RequestId}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := New(handler, sink)
	a.Start(ctx)

	for i := 0; i < n; i++ {
		if !a.Send(&pb.RemoteEnvelope{RequestId: uint64(i + 1)}) {
			t.Fatalf("Send(%d) returned false", i)
		}
	}

	results := sink.waitN(t, n, time.Second)
	for i := 0; i < n; i++ {
		if results[i].RequestId != uint64(i+1) {
			t.Fatalf("result[%d] expected request_id=%d, got %d", i, i+1, results[i].RequestId)
		}
	}
}

func TestActor_MultipleProducersProcessAllMessages(t *testing.T) {
	t.Parallel()

	const n = 128
	sink := newRecordingSink(n)
	var handled atomic.Int64
	handler := handlerFunc(func(env *pb.RemoteEnvelope) *pb.RemoteResult {
		handled.Add(1)
		return &pb.RemoteResult{Success: true, RequestId: env.RequestId}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := New(handler, sink)
	a.Start(ctx)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			if !a.Send(&pb.RemoteEnvelope{RequestId: uint64(i + 1)}) {
				t.Errorf("Send(%d) returned false", i)
			}
			wg.Done()
		}()
	}
	wg.Wait()

	_ = sink.waitN(t, n, time.Second)
	if got := handled.Load(); got != n {
		t.Fatalf("expected %d handled messages, got %d", n, got)
	}
}

func TestActor_HandlerDoesNotRunConcurrently(t *testing.T) {
	t.Parallel()

	const n = 16
	sink := newRecordingSink(n)
	var active atomic.Int64
	var maxActive atomic.Int64
	handler := handlerFunc(func(env *pb.RemoteEnvelope) *pb.RemoteResult {
		cur := active.Add(1)
		for {
			old := maxActive.Load()
			if cur <= old || maxActive.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return &pb.RemoteResult{Success: true, RequestId: env.RequestId}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := New(handler, sink)
	a.Start(ctx)

	for i := 0; i < n; i++ {
		if !a.Send(&pb.RemoteEnvelope{RequestId: uint64(i + 1)}) {
			t.Fatalf("Send(%d) returned false", i)
		}
	}
	_ = sink.waitN(t, n, time.Second)

	if got := maxActive.Load(); got != 1 {
		t.Fatalf("handler ran concurrently, max active=%d", got)
	}
}

func TestActor_EmitsToActorSink(t *testing.T) {
	t.Parallel()

	sink := newRecordingSink(1)
	handler := handlerFunc(func(*pb.RemoteEnvelope) *pb.RemoteResult {
		return &pb.RemoteResult{Success: true}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := New(handler, sink)
	a.Start(ctx)

	if !a.Send(&pb.RemoteEnvelope{RequestId: 1}) {
		t.Fatal("Send returned false")
	}
	res := sink.wait(t, time.Second)
	if !res.Success || res.RequestId != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestActor_StopRejectsNewMessages(t *testing.T) {
	t.Parallel()

	a := New(handlerFunc(func(*pb.RemoteEnvelope) *pb.RemoteResult {
		return &pb.RemoteResult{Success: true}
	}), newRecordingSink(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	a.Stop()

	deadline := time.Now().Add(time.Second)
	for a.IsAlive() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.IsAlive() {
		t.Fatal("actor stayed alive after Stop")
	}
	if a.Send(&pb.RemoteEnvelope{}) {
		t.Fatal("Send returned true after Stop")
	}
}

func TestActor_StopDrainsQueuedMessagesBeforeExit(t *testing.T) {
	t.Parallel()

	sink := newRecordingSink(2)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	handler := handlerFunc(func(env *pb.RemoteEnvelope) *pb.RemoteResult {
		if env.RequestId == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return &pb.RemoteResult{Success: true, RequestId: env.RequestId}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := New(handler, sink)
	a.Start(ctx)

	if !a.Send(&pb.RemoteEnvelope{RequestId: 1}) {
		t.Fatal("Send(first) returned false")
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first message did not start handling")
	}
	if !a.Send(&pb.RemoteEnvelope{RequestId: 2}) {
		t.Fatal("Send(second) returned false")
	}

	a.Stop()
	close(releaseFirst)

	results := sink.waitN(t, 2, time.Second)
	if results[0].RequestId != 1 || results[1].RequestId != 2 {
		t.Fatalf("expected queued messages to drain in order, got %+v", results)
	}
}

func TestActor_ContextCancelStopsActor(t *testing.T) {
	t.Parallel()

	a := New(handlerFunc(func(*pb.RemoteEnvelope) *pb.RemoteResult {
		return &pb.RemoteResult{Success: true}
	}), newRecordingSink(1))
	ctx, cancel := context.WithCancel(context.Background())
	a.Start(ctx)
	cancel()

	deadline := time.Now().Add(time.Second)
	for a.IsAlive() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.IsAlive() {
		t.Fatal("actor stayed alive after context cancel")
	}
}

func TestActor_NewPanicsOnNilDependencies(t *testing.T) {
	t.Parallel()

	assertPanic(t, func() {
		New(nil, newRecordingSink(1))
	})
	assertPanic(t, func() {
		New(handlerFunc(func(*pb.RemoteEnvelope) *pb.RemoteResult {
			return &pb.RemoteResult{Success: true}
		}), nil)
	})
}

func TestPutEnvelopeClearsRetainedFields(t *testing.T) {
	t.Parallel()

	qnode := GetEnvelope()
	qnode.Msg = &pb.RemoteEnvelope{
		TenantId:  1,
		Uid:       2,
		RequestId: 3,
		TxId:      "tx",
		OpCode:    4,
		Payload:   []byte("payload"),
	}
	qnode.next.Store(&Envelope{})

	PutEnvelope(qnode)
	if qnode.Msg != nil || qnode.next.Load() != nil {
		t.Fatalf("envelope was not cleared before reuse: msg=%v next=%v", qnode.Msg, qnode.next.Load())
	}
}

type recordingSink struct {
	ch chan pb.RemoteResult
}

type handlerFunc func(env *pb.RemoteEnvelope) *pb.RemoteResult

func (f handlerFunc) Handle(env *pb.RemoteEnvelope) *pb.RemoteResult {
	return f(env)
}

func assertPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}

func newRecordingSink(capacity int) *recordingSink {
	return &recordingSink{ch: make(chan pb.RemoteResult, capacity)}
}

func (s *recordingSink) Emit(result *pb.RemoteResult) {
	if result != nil {
		s.ch <- *result
	}
}

func (s *recordingSink) wait(t *testing.T, timeout time.Duration) pb.RemoteResult {
	t.Helper()
	select {
	case res := <-s.ch:
		return res
	case <-time.After(timeout):
		t.Fatal("timed out waiting for result")
		return pb.RemoteResult{}
	}
}

func (s *recordingSink) waitN(t *testing.T, n int, timeout time.Duration) []pb.RemoteResult {
	t.Helper()
	results := make([]pb.RemoteResult, 0, n)
	deadline := time.After(timeout)
	for len(results) < n {
		select {
		case res := <-s.ch:
			results = append(results, res)
		case <-deadline:
			t.Fatalf("timed out waiting for %d results, got %d", n, len(results))
		}
	}
	return results
}
