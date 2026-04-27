package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
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
	drainWindow time.Duration
	uidMin      int64
	uidMax      int64
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
	baseline   metricsBaseline
}

type metricsBaseline struct {
	sent           uint64
	success        uint64
	errors         uint64
	latencyNanos   uint64
	latencySamples uint64
	errorBreakdown map[string]uint64
}

func runStressCommand(args []string) error {
	var cfg stressConfig
	fs := flag.NewFlagSet("stress", flag.ContinueOnError)
	registerSharedFlags(fs, &cfg.clientConfig)
	fs.IntVar(&cfg.tpsTarget, "tps", 50000, "target overall TPS across all clients")
	fs.IntVar(&cfg.concurrency, "concurrency", 5000, "virtual client sockets concurrency")
	fs.DurationVar(&cfg.duration, "duration", 30*time.Second, "test duration")
	fs.DurationVar(&cfg.drainWindow, "drain-window", 60*time.Second, "how long to wait for in-flight requests to drain after traffic stops")
	fs.Int64Var(&cfg.uidMin, "uid-min", 1, "minimum uid (inclusive) used by the random load generator")
	fs.Int64Var(&cfg.uidMax, "uid-max", 100, "maximum uid (inclusive) used by the random load generator")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := cfg.normalize(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	router, cleanup, err := newRouterFromConfig(ctx, cfg.clientConfig)
	if err != nil {
		return err
	}
	defer cleanup()

	log.Printf(
		"Starting Load Generator: target %d TPS, concurrency %d, batch %d, flush delay %s, uid range [%d,%d], drain window %s",
		cfg.tpsTarget,
		cfg.concurrency,
		cfg.batchSize,
		cfg.flushDelay,
		cfg.uidMin,
		cfg.uidMax,
		cfg.drainWindow,
	)
	stopMetrics := startMetricsReporter(ctx, router, func(sentTPS, successTPS, errorTPS, inFlight, callbackPending uint64) {
		log.Printf("Live: Sent(TPS): %d | Success(TPS): %d | Error(TPS): %d | InFlight: %d | PendingCallbacks(total): %d | Totals sent=%d success=%d error=%d",
			sentTPS,
			successTPS,
			errorTPS,
			inFlight,
			callbackPending,
			router.reqSent.Load(),
			router.respRecv.Load(),
			router.errCount.Load(),
		)
	})
	defer stopMetrics()

	runCtx, cancel := context.WithTimeout(ctx, cfg.duration)
	defer cancel()

	generationStartedAt := time.Now()
	runStressGenerators(runCtx, router, cfg)
	generationDuration := time.Since(generationStartedAt)
	log.Println("Traffic generation duration completed. Waiting for in-flight responses to drain...")
	drainStartedAt := time.Now()
	drained, abandoned := drainInFlight(router, cfg.drainWindow)
	drainDuration := time.Since(drainStartedAt)
	totalCompletionDuration := generationDuration + drainDuration
	totalSent := router.reqSent.Load()
	totalSuccess := router.respRecv.Load()
	offeredTPS := throughputPerSecond(totalSent, generationDuration)
	completedTPS := throughputPerSecond(totalSuccess, totalCompletionDuration)

	log.Printf("===== Load Test Finished =====")
	log.Printf("Config: tps=%d concurrency=%d batch=%d flush_delay=%s duration=%s drain_window=%s uid_range=[%d,%d]",
		cfg.tpsTarget,
		cfg.concurrency,
		cfg.batchSize,
		cfg.flushDelay,
		cfg.duration,
		cfg.drainWindow,
		cfg.uidMin,
		cfg.uidMax,
	)
	log.Printf("Generation Duration: %s", generationDuration)
	log.Printf("Drain Duration: %s", drainDuration)
	log.Printf("Total Completion Duration: %s", totalCompletionDuration)
	log.Printf("Offered TPS: %.2f", offeredTPS)
	log.Printf("Completed TPS: %.2f", completedTPS)
	log.Printf("Pending Callbacks (total): %d", router.CallbackPending())
	log.Printf("Total Sent Envelopes: %d", totalSent)
	log.Printf("Total Success Responses: %d", totalSuccess)
	log.Printf("Total Errors: %d", router.errCount.Load())
	log.Printf("Drain Complete: %t", drained)
	log.Printf("InFlight Unfinished: %d", abandoned)
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
	if err := cfg.normalize(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.cancel = cancel
	e.done = done
	e.currentTPS.Store(0)
	e.running.Store(true)
	e.baseline = metricsBaseline{}

	go func() {
		defer close(done)
		defer e.running.Store(false)

		stopMetrics := startMetricsReporter(ctx, e.router, func(sentTPS, _, _, _, _ uint64) {
			e.currentTPS.Store(sentTPS)
		})
		defer stopMetrics()

		runCtx, stopRun := context.WithTimeout(ctx, cfg.duration)
		defer stopRun()

		runStressGenerators(runCtx, e.router, cfg)
		drainInFlight(e.router, cfg.drainWindow)
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
	e.mu.Lock()
	baseline := e.baseline
	e.mu.Unlock()

	currSent := e.router.reqSent.Load()
	currSuccess := e.router.respRecv.Load()
	currErrors := e.router.errCount.Load()
	currLatencyNanos := e.router.latencyNanos.Load()
	currLatencySamples := e.router.latencySamples.Load()
	currBreakdown := e.router.ErrorBreakdownMap()

	status := stressStatus{
		IsRunning:      e.running.Load(),
		TotalSent:      subtractWithFloor(currSent, baseline.sent),
		TotalSuccess:   subtractWithFloor(currSuccess, baseline.success),
		TotalErrors:    subtractWithFloor(currErrors, baseline.errors),
		CurrentTPS:     e.currentTPS.Load(),
		AvgLatencyMS:   diffAvgLatency(currLatencyNanos, currLatencySamples, baseline),
		ErrorBreakdown: diffErrorBreakdown(currBreakdown, baseline.errorBreakdown),
	}
	return status
}

func (e *StressEngine) ResetMetrics() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.baseline = metricsBaseline{
		sent:           e.router.reqSent.Load(),
		success:        e.router.respRecv.Load(),
		errors:         e.router.errCount.Load(),
		latencyNanos:   e.router.latencyNanos.Load(),
		latencySamples: e.router.latencySamples.Load(),
		errorBreakdown: e.router.ErrorBreakdownMap(),
	}
	e.currentTPS.Store(0)
}

func runStressGenerators(ctx context.Context, router *Router, cfg stressConfig) {
	if cfg.concurrency <= 0 || cfg.tpsTarget <= 0 {
		return
	}

	pacing := time.Duration(int64(time.Second) * int64(cfg.concurrency) / int64(cfg.tpsTarget))
	if pacing <= 0 {
		pacing = time.Nanosecond
	}

	uidSpan := cfg.uidMax - cfg.uidMin + 1
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
					uid := cfg.uidMin + rand.Int63n(uidSpan)
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

func (cfg *stressConfig) normalize() error {
	if cfg.drainWindow <= 0 {
		cfg.drainWindow = 60 * time.Second
	}
	if cfg.uidMin == 0 && cfg.uidMax == 0 {
		cfg.uidMin = 1
		cfg.uidMax = 100
	}
	if cfg.uidMin <= 0 {
		return fmt.Errorf("uid-min must be > 0")
	}
	if cfg.uidMax < cfg.uidMin {
		return fmt.Errorf("uid-max must be >= uid-min")
	}
	return nil
}

func drainInFlight(router *Router, timeout time.Duration) (drained bool, unfinished uint64) {
	deadline := time.Now().Add(timeout)
	for {
		sent := router.reqSent.Load()
		handled := router.respRecv.Load() + router.errCount.Load()
		if handled >= sent {
			return true, 0
		}
		if time.Now().After(deadline) {
			unfinished = sent - handled
			log.Printf("Drain timeout! %d requests are still in-flight after %s.", unfinished, timeout)
			return false, unfinished
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func startMetricsReporter(ctx context.Context, router *Router, onTick func(sentTPS, successTPS, errorTPS, inFlight, callbackPending uint64)) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		var lastSent uint64
		var lastSuccess uint64
		var lastErrors uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				currSent := router.reqSent.Load()
				currSuccess := router.respRecv.Load()
				currErrors := router.errCount.Load()
				sentTPS := currSent - lastSent
				successTPS := currSuccess - lastSuccess
				errorTPS := currErrors - lastErrors
				handled := currSuccess + currErrors
				var inFlight uint64
				if currSent > handled {
					inFlight = currSent - handled
				}
				callbackPending := router.CallbackPending()
				lastSent = currSent
				lastSuccess = currSuccess
				lastErrors = currErrors
				onTick(sentTPS, successTPS, errorTPS, inFlight, callbackPending)
			}
		}
	}()

	return func() {
		close(stop)
	}
}

func subtractWithFloor(curr, base uint64) uint64 {
	if curr < base {
		return 0
	}
	return curr - base
}

func diffAvgLatency(currNanos, currSamples uint64, baseline metricsBaseline) float64 {
	nanos := subtractWithFloor(currNanos, baseline.latencyNanos)
	samples := subtractWithFloor(currSamples, baseline.latencySamples)
	if samples == 0 {
		return 0
	}
	return float64(time.Duration(nanos/samples)) / float64(time.Millisecond)
}

func diffErrorBreakdown(curr, base map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64)
	for key, value := range curr {
		baseValue := base[key]
		if value <= baseValue {
			continue
		}
		out[key] = value - baseValue
	}
	return out
}

func throughputPerSecond(total uint64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(total) / d.Seconds()
}
