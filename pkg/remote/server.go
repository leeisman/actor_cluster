package remote

import (
	"context"
	"io"
	"sync"

	"github.com/frankieli/actor_cluster/pkg/remote/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultMaxBatchSize = 256

// ServerConfig controls transport-level stream guards.
type ServerConfig struct {
	MaxBatchSize int
}

// Server adapts ActorService streams to a node-level BatchHandler.
//
// The remote package owns only gRPC transport mechanics: receive batches,
// invoke the injected handler, and serialize outbound BatchResponse writes.
// Actor routing, ownership checks, dispatch ordering, and response batching
// intentionally live outside this package.
type Server struct {
	pb.UnimplementedActorServiceServer
	handler BatchHandler
	cfg     ServerConfig
}

// NewServer constructs a remote transport adapter.
func NewServer(handler BatchHandler, cfg ServerConfig) *Server {
	if handler == nil {
		panic("remote: nil BatchHandler")
	}
	if cfg.MaxBatchSize <= 0 {
		cfg.MaxBatchSize = defaultMaxBatchSize
	}
	return &Server{
		handler: handler,
		cfg:     cfg,
	}
}

// StreamMessages consumes BatchRequest values and delegates them to the handler.
func (s *Server) StreamMessages(stream pb.ActorService_StreamMessagesServer) error {
	sender := &streamBatchSender{stream: stream}
	ctx := stream.Context()

	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return status.Error(codes.Unavailable, ErrTransportClosed)
		}
		if batch == nil {
			return status.Error(codes.InvalidArgument, ErrBadRequest)
		}
		if len(batch.Envelopes) > s.cfg.MaxBatchSize {
			return status.Error(codes.InvalidArgument, ErrBatchTooLarge)
		}
		if handleErr := s.handler.HandleBatch(ctx, batch, sender); handleErr != nil {
			return normalizeHandlerError(handleErr)
		}
	}
}

type streamBatchSender struct {
	stream pb.ActorService_StreamMessagesServer
	mu     sync.Mutex
}

func (s *streamBatchSender) SendBatchResponse(resp *pb.BatchResponse) error {
	if resp == nil {
		return status.Error(codes.InvalidArgument, ErrBadRequest)
	}
	s.mu.Lock()
	err := s.stream.Send(resp)
	s.mu.Unlock()
	if err != nil {
		return status.Error(codes.Unavailable, ErrTransportClosed)
	}
	return nil
}

func normalizeHandlerError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if err == context.Canceled {
		return status.Error(codes.Canceled, ErrTransportClosed)
	}
	if err == context.DeadlineExceeded {
		return status.Error(codes.DeadlineExceeded, ErrDeadlineExceeded)
	}
	return status.Error(codes.Internal, ErrInternal)
}
