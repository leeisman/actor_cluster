package main

import (
	"context"
	"encoding/binary"
	"flag"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/frankieli/actor_cluster/pkg/remote/pb"
)

type stressConfig struct {
	clientConfig
	tpsTarget   int
	concurrency int
	duration    time.Duration
}

type stressStatus struct {
	IsRunning      bool              `json:"is_running"`
	TotalSent      uint64            `json:"total_sent"`
	TotalSuccess   uint64            `json:"total_success"`
	TotalErrors    uint64            `json:"total_errors"`
	CurrentTPS     uint64            `json:"current_tps"`
	AvgLatencyMS   float64           `json:"avg_latency_ms"`
	ErrorBreakdown map[string]uint64 `json:"error_breakdown"`
}

type StressEngine struct {
	router *Router

	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	running    atomic.Bool
	currentTPS atomic.Uint64
}

func runStressCommand(args []string) error {
	var cfg stressConfig
	fs := flag.NewFlagSet("stress", flag.ContinueOnError)
	registerSharedFlags(fs, &cfg.clientConfig)
	fs.IntVar(&cfg.tpsTarget, "tps", 50000, "target overall TPS across all clients")
	fs.IntVar(&cfg.concurrency, "concurrency", 5000, "virtual client sockets concurrency")
	fs.DurationVar(&cfg.duration, "duration", 30*time.Second, "test duration")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	router, cleanup, err := newRouterFromConfig(ctx, cfg.clientConfig)
	if err != nil {
		return err
	}
	defer cleanup()

	log.Printf("Starting Load Generator: target %d TPS, concurrency %d", cfg.tpsTarget, cfg.concurrency)
	stopMetrics := startMetricsReporter(ctx, router, func(tps uint64) {
		log.Printf("Live: Sent(TPS): %d | Success(TPS): %d | Error(TPS): %d | Total Sent: %d",
			tps,
			router.respRecv.Load(),
			router.errCount.Load(),
			router.reqSent.Load(),
		)
	})
	defer stopMetrics()

	runCtx, cancel := context.WithTimeout(ctx, cfg.duration)
	defer cancel()

	runStressGenerators(runCtx, router, cfg)
	log.Println("Traffic generation duration completed. Waiting for in-flight responses to drain...")
	drainInFlight(router, 15*time.Second)

	log.Printf("===== Load Test Finished =====")
	log.Printf("Total Sent Envelopes: %d", router.reqSent.Load())
	log.Printf("Total Success Responses: %d", router.respRecv.Load())
	log.Printf("Total Errors: %d", router.errCount.Load())
	log.Printf("Error Breakdown: %v", router.ErrorBreakdownMap())

	return nil
}

func NewStressEngine(router *Router) *StressEngine {
	return &StressEngine{router: router}
}

func (e *StressEngine) Start(cfg stressConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running.Load() {
		return errAlreadyRunning
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.cancel = cancel
	e.done = done
	e.currentTPS.Store(0)
	e.running.Store(true)

	go func() {
		defer close(done)
		defer e.running.Store(false)

		stopMetrics := startMetricsReporter(ctx, e.router, func(tps uint64) {
			e.currentTPS.Store(tps)
		})
		defer stopMetrics()

		runCtx, stopRun := context.WithTimeout(ctx, cfg.duration)
		defer stopRun()

		runStressGenerators(runCtx, e.router, cfg)
		drainInFlight(e.router, 5*time.Second)
		e.currentTPS.Store(0)
	}()

	return nil
}

func (e *StressEngine) Stop() {
	e.mu.Lock()
	cancel := e.cancel
	done := e.done
	e.cancel = nil
	e.done = nil
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (e *StressEngine) Status() stressStatus {
	return stressStatus{
		IsRunning:      e.running.Load(),
		TotalSent:      e.router.reqSent.Load(),
		TotalSuccess:   e.router.respRecv.Load(),
		TotalErrors:    e.router.errCount.Load(),
		CurrentTPS:     e.currentTPS.Load(),
		AvgLatencyMS:   float64(e.router.AvgLatency()) / float64(time.Millisecond),
		ErrorBreakdown: e.router.ErrorBreakdownMap(),
	}
}

func runStressGenerators(ctx context.Context, router *Router, cfg stressConfig) {
	if cfg.concurrency <= 0 || cfg.tpsTarget <= 0 {
		return
	}

	pacing := time.Duration(int64(time.Second) * int64(cfg.concurrency) / int64(cfg.tpsTarget))
	if pacing <= 0 {
		pacing = time.Nanosecond
	}

	var wg sync.WaitGroup
	for i := 0; i < cfg.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			ticker := time.NewTicker(pacing)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					reqID := nextRequestID()
					uid := rand.Int63n(100) + 1
					payloadBuf := make([]byte, 8)
					binary.BigEndian.PutUint64(payloadBuf, 1)
					env := &pb.RemoteEnvelope{
						TenantId:  1,
						Uid:       uid,
						RequestId: reqID,
						TxId:      strconv.FormatUint(reqID, 10),
						OpCode:    1,
						Payload:   payloadBuf,
					}
					router.EmulateGatewayFlow(1, uid, reqID, env)
				}
			}
		}()
	}
	wg.Wait()
}

func drainInFlight(router *Router, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		sent := router.reqSent.Load()
		handled := router.respRecv.Load() + router.errCount.Load()
		if handled >= sent {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("Drain timeout! Abandoning %d in-flight requests...", sent-handled)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func startMetricsReporter(ctx context.Context, router *Router, onTPS func(uint64)) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		var lastSent uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				currSent := router.reqSent.Load()
				tps := currSent - lastSent
				lastSent = currSent
				onTPS(tps)
			}
		}
	}()

	return func() {
		close(stop)
	}
}
