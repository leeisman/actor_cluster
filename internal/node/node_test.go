package node

import (
	"context"
	"testing"
	"time"

	"github.com/frankieli/actor_cluster/pkg/remote"
	pb "github.com/frankieli/actor_cluster/pkg/remote/pb"
)

type stubResolver struct {
	ip  string
	err error
}

func (r *stubResolver) GetNodeIP(int32, int64) (string, error) {
	return r.ip, r.err
}

func (r *stubResolver) Watch(context.Context) error { return nil }

func (r *stubResolver) Close() error { return nil }

type recordingBatchSender struct {
	ch chan *pb.BatchResponse
}

func newRecordingBatchSender() *recordingBatchSender {
	return &recordingBatchSender{ch: make(chan *pb.BatchResponse, 8)}
}

func (s *recordingBatchSender) SendBatchResponse(resp *pb.BatchResponse) error {
	s.ch <- resp
	return nil
}

func (s *recordingBatchSender) waitResult(t *testing.T) *pb.RemoteResult {
	t.Helper()

	select {
	case resp := <-s.ch:
		if resp == nil || len(resp.Results) == 0 {
			t.Fatal("expected at least one result in batch response")
		}
		return resp.Results[0]
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for batch response")
		return nil
	}
}

func TestNodeHandleBatch_RejectsMissingRequestID(t *testing.T) {
	t.Parallel()

	n := NewNode(&mockStore{}, nil, "")
	defer n.Stop()

	sender := newRecordingBatchSender()
	err := n.HandleBatch(context.Background(), &pb.BatchRequest{
		Envelopes: []*pb.RemoteEnvelope{{
			TenantId: 1,
			Uid:      42,
			TxId:     "tx-no-request-id",
		}},
	}, sender)
	if err != nil {
		t.Fatalf("HandleBatch returned error: %v", err)
	}

	res := sender.waitResult(t)
	if res.ErrorCode != remote.ErrBadRequest {
		t.Fatalf("expected error_code=%s, got %s", remote.ErrBadRequest, res.ErrorCode)
	}
	if res.Success {
		t.Fatalf("expected failure result, got success: %+v", res)
	}
}

func TestNodeHandleBatch_WrongNodeUsesStableErrorCode(t *testing.T) {
	t.Parallel()

	n := NewNode(&mockStore{}, &stubResolver{ip: "10.0.0.2:50051"}, "10.0.0.1:50051")
	defer n.Stop()

	sender := newRecordingBatchSender()
	err := n.HandleBatch(context.Background(), &pb.BatchRequest{
		Envelopes: []*pb.RemoteEnvelope{{
			TenantId:  1,
			Uid:       42,
			TxId:      "tx-wrong-node",
			RequestId: 99,
			OpCode:    7,
		}},
	}, sender)
	if err != nil {
		t.Fatalf("HandleBatch returned error: %v", err)
	}

	res := sender.waitResult(t)
	if res.ErrorCode != remote.ErrWrongNode {
		t.Fatalf("expected error_code=%s, got %s", remote.ErrWrongNode, res.ErrorCode)
	}
	if res.OpCode != 7 {
		t.Fatalf("expected op_code to be preserved, got %d", res.OpCode)
	}
}
