package remote

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/frankieli/actor_cluster/pkg/remote/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestStreamMessages_DelegatesBatchAndSendsResponse(t *testing.T) {
	t.Parallel()

	calls := atomic.Int64{}
	server := NewServer(
		batchHandlerFunc(func(_ context.Context, batch *pb.BatchRequest, sender BatchSender) error {
			calls.Add(1)
			if len(batch.Envelopes) != 2 {
				t.Fatalf("expected 2 envelopes, got %d", len(batch.Envelopes))
			}
			return sender.SendBatchResponse(&pb.BatchResponse{
				Results: []*pb.RemoteResult{
					{RequestId: batch.Envelopes[0].RequestId, Success: true},
					{RequestId: batch.Envelopes[1].RequestId, Success: true},
				},
			})
		}),
		ServerConfig{MaxBatchSize: 8},
	)

	stream := newMockStream([]*pb.BatchRequest{
		{
			Envelopes: []*pb.RemoteEnvelope{
				{RequestId: 101},
				{RequestId: 102},
			},
		},
	})

	if err := server.StreamMessages(stream); err != nil {
		t.Fatalf("StreamMessages returned error: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 handler call, got %d", got)
	}

	results := flattenResults(stream.sent)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	assertResult(t, results, 101)
	assertResult(t, results, 102)
}

func TestStreamMessages_BatchTooLargeStopsBeforeHandler(t *testing.T) {
	t.Parallel()

	calls := atomic.Int64{}
	server := NewServer(
		batchHandlerFunc(func(context.Context, *pb.BatchRequest, BatchSender) error {
			calls.Add(1)
			return nil
		}),
		ServerConfig{MaxBatchSize: 1},
	)

	stream := newMockStream([]*pb.BatchRequest{
		{
			Envelopes: []*pb.RemoteEnvelope{
				{RequestId: 1},
				{RequestId: 2},
			},
		},
	})

	err := server.StreamMessages(stream)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if got := status.Convert(err).Message(); got != ErrBatchTooLarge {
		t.Fatalf("expected %s, got %s", ErrBatchTooLarge, got)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("expected 0 handler calls, got %d", got)
	}
}

func TestStreamMessages_ConcurrentSenderCallsAreSerialized(t *testing.T) {
	t.Parallel()

	const sends = 32
	server := NewServer(
		batchHandlerFunc(func(_ context.Context, _ *pb.BatchRequest, sender BatchSender) error {
			var wg sync.WaitGroup
			wg.Add(sends)
			for i := 0; i < sends; i++ {
				go func(id int) {
					err := sender.SendBatchResponse(&pb.BatchResponse{
						Results: []*pb.RemoteResult{{RequestId: uint64(id + 1), Success: true}},
					})
					if err != nil {
						t.Errorf("SendBatchResponse failed: %v", err)
					}
					wg.Done()
				}(i)
			}
			wg.Wait()
			return nil
		}),
		ServerConfig{MaxBatchSize: 8},
	)

	stream := newMockStream([]*pb.BatchRequest{{Envelopes: []*pb.RemoteEnvelope{{RequestId: 1}}}})

	if err := server.StreamMessages(stream); err != nil {
		t.Fatalf("StreamMessages returned error: %v", err)
	}
	if len(stream.sent) != sends {
		t.Fatalf("expected %d responses, got %d", sends, len(stream.sent))
	}
}

func TestStreamMessages_NormalizesHandlerError(t *testing.T) {
	t.Parallel()

	server := NewServer(
		batchHandlerFunc(func(context.Context, *pb.BatchRequest, BatchSender) error {
			return context.DeadlineExceeded
		}),
		ServerConfig{MaxBatchSize: 8},
	)

	stream := newMockStream([]*pb.BatchRequest{{Envelopes: []*pb.RemoteEnvelope{{RequestId: 1}}}})

	err := server.StreamMessages(stream)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if got := status.Convert(err).Message(); got != ErrDeadlineExceeded {
		t.Fatalf("expected %s, got %s", ErrDeadlineExceeded, got)
	}
}

type batchHandlerFunc func(ctx context.Context, batch *pb.BatchRequest, sender BatchSender) error

func (f batchHandlerFunc) HandleBatch(ctx context.Context, batch *pb.BatchRequest, sender BatchSender) error {
	return f(ctx, batch, sender)
}

type mockStream struct {
	ctx     context.Context
	batches []*pb.BatchRequest
	recvIdx int
	sent    []*pb.BatchResponse
	mu      sync.Mutex
}

func newMockStream(batches []*pb.BatchRequest) *mockStream {
	return &mockStream{
		ctx:     context.Background(),
		batches: batches,
	}
}

func (m *mockStream) Context() context.Context {
	return m.ctx
}

func (m *mockStream) Recv() (*pb.BatchRequest, error) {
	if m.recvIdx >= len(m.batches) {
		return nil, io.EOF
	}
	b := m.batches[m.recvIdx]
	m.recvIdx++
	return b, nil
}

func (m *mockStream) Send(resp *pb.BatchResponse) error {
	m.mu.Lock()
	m.sent = append(m.sent, resp)
	m.mu.Unlock()
	return nil
}

func (m *mockStream) SetHeader(metadata.MD) error {
	return nil
}

func (m *mockStream) SendHeader(metadata.MD) error {
	return nil
}

func (m *mockStream) SetTrailer(metadata.MD) {}

func (m *mockStream) SendMsg(any) error {
	return nil
}

func (m *mockStream) RecvMsg(any) error {
	return nil
}

func flattenResults(responses []*pb.BatchResponse) []*pb.RemoteResult {
	out := make([]*pb.RemoteResult, 0, len(responses))
	for i := 0; i < len(responses); i++ {
		out = append(out, responses[i].Results...)
	}
	return out
}

func assertResult(t *testing.T, results []*pb.RemoteResult, requestID uint64) {
	t.Helper()
	for i := 0; i < len(results); i++ {
		if results[i].RequestId == requestID {
			return
		}
	}
	t.Fatalf("request_id=%d not found in results", requestID)
}
