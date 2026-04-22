package main

import (
	"context"
	"io"
	"log"
	"time"

	"github.com/frankieli/actor_cluster/pkg/remote"
	"github.com/frankieli/actor_cluster/pkg/remote/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// NodeStreamer 負責維護對單一 POD_IP 的 gRPC 長連線
type NodeStreamer struct {
	TargetIP   string
	grpcConn   *grpc.ClientConn
	stream     pb.ActorService_StreamMessagesClient
	envelopeCh chan *pb.RemoteEnvelope
	targetSize int
	flushDelay time.Duration

	callbackMap *ShardedCallbackMap
	router      *Router

	ctx    context.Context
	cancel context.CancelFunc
	stopCh chan struct{}
	done   chan struct{}
}

func NewNodeStreamer(
	ip string,
	targetSize int,
	flushDelay time.Duration,
	router *Router,
) (*NodeStreamer, error) {
	conn, err := grpc.Dial(ip, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	client := pb.NewActorServiceClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.StreamMessages(ctx)
	if err != nil {
		cancel()
		conn.Close()
		return nil, err
	}

	ns := &NodeStreamer{
		TargetIP:       ip,
		grpcConn:       conn,
		stream:         stream,
		envelopeCh:     make(chan *pb.RemoteEnvelope, targetSize*2),
		targetSize:     targetSize,
		flushDelay:     flushDelay,
		callbackMap:    NewShardedCallbackMap(),
		router:         router,
		ctx:            ctx,
		cancel:         cancel,
		stopCh:         make(chan struct{}),
		done:           make(chan struct{}),
	}

	go ns.flusherLoop()
	go ns.receiverLoop()

	return ns, nil
}

func (ns *NodeStreamer) sendBatch(batch []*pb.RemoteEnvelope) {
	if len(batch) == 0 {
		return
	}
	req := &pb.BatchRequest{Envelopes: batch}
	if err := ns.stream.Send(req); err != nil {
		if err != io.EOF {
			log.Printf("[Streamer %s] Send error: %v", ns.TargetIP, err)
		}
		// 精準回收整批失敗的 Envelope，不讓記憶體洩漏
		ns.router.errCount.Add(uint64(len(batch)))
		for _, env := range batch {
			ns.callbackMap.LoadAndDelete(env.RequestId)
			ns.router.RecordError(remote.ErrTransportClosed)
		}
	} else {
		ns.router.reqSent.Add(uint64(len(batch)))
	}
}

func (ns *NodeStreamer) flusherLoop() {
	ticker := time.NewTicker(ns.flushDelay)
	defer ticker.Stop()

	// 預先分配大容量陣列，Flush 後清空但不釋放 (Zero-allocation)
	batch := make([]*pb.RemoteEnvelope, 0, ns.targetSize)

	for {
		select {
		case <-ns.stopCh:
			ns.sendBatch(batch)
			ns.stream.CloseSend() // 關閉送出流，通知 Server EOF
			return
		case env := <-ns.envelopeCh:
			batch = append(batch, env)
			if len(batch) >= ns.targetSize {
				ns.sendBatch(batch)
				batch = batch[:0] // Retain capacity
				ticker.Reset(ns.flushDelay)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				ns.sendBatch(batch)
				batch = batch[:0]
			}
		}
	}
}

func (ns *NodeStreamer) receiverLoop() {
	defer close(ns.done)
	defer ns.grpcConn.Close()

	for {
		resp, err := ns.stream.Recv()
		if err != nil {
			if err == io.EOF {
				return
			}
			log.Printf("[Streamer %s] Recv error: %v", ns.TargetIP, err)
			ns.callbackMap.FailAll(ns.router, remote.ErrStreamInterrupted)
			return
		}

		for _, res := range resp.Results {
			if res.Success {
				ns.router.respRecv.Add(1)
			} else {
				ns.router.errCount.Add(1)
				errKey := res.ErrorCode
				if res.ErrorMsg != "" {
					errKey = res.ErrorCode + " (" + res.ErrorMsg + ")"
				}
				ns.router.RecordError(errKey)
			}

			// Demultiplexing & Latency Tracking: O(1) 內找到對應這筆 ReqID 的出發時間
			if startedAt, ok := ns.callbackMap.LoadAndDelete(res.RequestId); ok {
				ns.router.RecordLatency(time.Since(startedAt))
			}
		}
	}
}

func (ns *NodeStreamer) StopAndWait() {
	close(ns.stopCh)
	<-ns.done
	ns.cancel()
}
