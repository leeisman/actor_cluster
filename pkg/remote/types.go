package remote

import (
	"context"

	"github.com/frankieli/actor_cluster/pkg/remote/pb"
)

// BatchHandler receives transport batches from a single gRPC stream.
//
// Implementations own actor routing, validation, dispatch ordering, response
// aggregation, and any goroutine lifecycle they start. Background work must
// observe ctx and stop using sender after the stream closes.
type BatchHandler interface {
	HandleBatch(ctx context.Context, batch *pb.BatchRequest, sender BatchSender) error
}

// BatchSender is the transport-owned response outlet for one gRPC stream.
//
// SendBatchResponse is safe for concurrent calls; the remote package
// serializes writes because gRPC stream sends must not race each other.
type BatchSender interface {
	SendBatchResponse(resp *pb.BatchResponse) error
}
