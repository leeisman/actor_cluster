package node

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/frankieli/actor_cluster/pkg/discovery"
	pb "github.com/frankieli/actor_cluster/pkg/remote/pb"
)

// staticResolver 僅用於測試：所有 key 皆導向同一 myIP。
type staticResolver struct {
	ip string
}

func (s *staticResolver) GetNodeIP(int32, int64) (string, error) {
	return s.ip, nil
}

func (s *staticResolver) Watch(context.Context) error { return nil }

func (s *staticResolver) Close() error { return nil }

var _ discovery.TopologyResolver = (*staticResolver)(nil)

type recordingSender struct {
	mu    sync.Mutex
	batch []*pb.RemoteResult
}

func (r *recordingSender) SendBatchResponse(resp *pb.BatchResponse) error {
	if resp == nil {
		return nil
	}
	r.mu.Lock()
	r.batch = append(r.batch, resp.Results...)
	r.mu.Unlock()
	return nil
}

func (r *recordingSender) snapshot() []*pb.RemoteResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*pb.RemoteResult, len(r.batch))
	copy(out, r.batch)
	return out
}

func TestNode_ConcurrentStreams_ReplyToCorrectBatchSender(t *testing.T) {
	myIP := "127.0.0.1:50051"
	store := &mockStore{
		snapshot: &snapshotRow{balance: 0, lastVersion: 0},
		events:   nil,
	}
	n := NewNode(store, &staticResolver{ip: myIP}, myIP, Config{})
	defer n.Stop()

	ctx := context.Background()
	s1 := &recordingSender{}
	s2 := &recordingSender{}

	envA := &pb.RemoteEnvelope{
		TenantId: 1, Uid: 100, RequestId: 1, TxId: "tx-a", OpCode: 1,
		Payload: encodeAmount(10),
	}
	envB := &pb.RemoteEnvelope{
		TenantId: 1, Uid: 200, RequestId: 2, TxId: "tx-b", OpCode: 1,
		Payload: encodeAmount(5),
	}

	var wg sync.WaitGroup
	var handleErr1, handleErr2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		handleErr1 = n.HandleBatch(ctx, &pb.BatchRequest{Envelopes: []*pb.RemoteEnvelope{envA}}, s1)
	}()
	go func() {
		defer wg.Done()
		handleErr2 = n.HandleBatch(ctx, &pb.BatchRequest{Envelopes: []*pb.RemoteEnvelope{envB}}, s2)
	}()
	wg.Wait()
	if handleErr1 != nil {
		t.Fatalf("HandleBatch s1: %v", handleErr1)
	}
	if handleErr2 != nil {
		t.Fatalf("HandleBatch s2: %v", handleErr2)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a := recordingSender{}
		a.batch = nil
		b1 := s1.snapshot()
		b2 := s2.snapshot()
		if len(b1) >= 1 && len(b2) >= 1 {
			if b1[0].RequestId != 1 || b1[0].Uid != 100 {
				t.Fatalf("stream1 got wrong result: req=%d uid=%d", b1[0].RequestId, b1[0].Uid)
			}
			if b2[0].RequestId != 2 || b2[0].Uid != 200 {
				t.Fatalf("stream2 got wrong result: req=%d uid=%d", b2[0].RequestId, b2[0].Uid)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for both streams; s1 n=%d s2 n=%d", len(s1.snapshot()), len(s2.snapshot()))
}
