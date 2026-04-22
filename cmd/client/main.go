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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/frankieli/actor_cluster/pkg/discovery" // 無 Registry；NewEtcdResolver + Watch 後僅 GetNodeIP（與 cmd/node 之 resolver 前綴/語意一致，見 docs/design/01_discovery_spec.md §14）
	"github.com/frankieli/actor_cluster/pkg/remote/pb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	etcdEndpoints = flag.String("etcd", "127.0.0.1:2379", "etcd endpoints")
	nodePrefix    = flag.String("node-prefix", "/actor_cluster/nodes", "etcd key prefix")
	tpsTarget     = flag.Int("tps", 50000, "Target overall TPS across all clients")
	batchSize     = flag.Int("batch", 1000, "Max batch size for sending")
	concurrency   = flag.Int("concurrency", 5000, "Virtual client sockets concurrency")
	duration      = flag.Duration("duration", 30*time.Second, "Test duration")
)

var reqIDGenerator atomic.Uint64

func init() {
	// 無鎖類雪花算法 (Lock-free Snowflake-like) 機制：
	// 為避免多台 Client Pods 同時產生相同的 ReqID (從 0 開始)，導致在同一台 Node 的 replyRoutes 發生 Map Key 覆蓋。
	// 我們將前 16 bits 設為隨機 MachineID，後 48 bits 留給 atomic 遞增序列。
	// 這樣保證了 65536 台發射機以內絕對不碰撞，且熱點路徑依然是極致的 atomic.AddUint64 零鎖開銷。
	rand.Seed(time.Now().UnixNano())
	machineID := uint64(rand.Intn(65536)) << 48
	reqIDGenerator.Store(machineID)
}

func main() {
	flag.Parse()

	// 1. Etcd Discovery（與 cmd/node 相同：endpoints 逗號分隔、*nodePrefix、clientv3.New、defer Close）：
	// 僅建 NewEtcdResolver+Watch，不註冊本機；路由數學僅在 pkg/discovery，禁止本程式實作 SlotOf/建表。
	endpoints := strings.Split(*etcdEndpoints, ",")
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("etcd connection failed: %v", err)
	}
	defer etcdCli.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resolver := discovery.NewEtcdResolver(etcdCli, *nodePrefix)
	if err := resolver.Watch(ctx); err != nil {
		log.Fatalf("resolver watch failed: %v", err)
	}

	router := NewRouter(resolver, *batchSize, 5*time.Millisecond)

	log.Printf("Starting Load Generator: Target %d TPS, Concurrency %d (Virtual Sockets)", *tpsTarget, *concurrency)

	// 2. Metrics Reporter (Zero-Lock, 1-second view)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var lastSent, lastRecv, lastErr uint64

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				currSent := router.reqSent.Load()
				currRecv := router.respRecv.Load()
				currErr := router.errCount.Load()

				sentPerSec := currSent - lastSent
				recvPerSec := currRecv - lastRecv
				errPerSec := currErr - lastErr

				lastSent = currSent
				lastRecv = currRecv
				lastErr = currErr

				log.Printf("Live: Sent(TPS): %d | Success(TPS): %d | Error(TPS): %d | Total Sent: %d",
					sentPerSec, recvPerSec, errPerSec, currSent)
				if errPerSec > 0 {
					log.Printf("Live Errors: %s", router.GetErrorBreakdown())
				}
				if avg := router.AvgLatency(); avg > 0 {
					log.Printf("Live: Avg Latency: %s", avg)
				}
			}
		}
	}()

	// 3. Spawns Virtual Client Sockets (Traffic Generator)
	var wg sync.WaitGroup
	// 計算每條 Goroutine 該間隔多久噴一發 (Rate Limiting)
	pacing := time.Duration(int64(time.Second) * int64(*concurrency) / int64(*tpsTarget))

	startTime := time.Now()

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()

			ticker := time.NewTicker(pacing)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if time.Since(startTime) > *duration {
						return
					}

					reqID := reqIDGenerator.Add(1)
					uid := rand.Int63n(100) + 1

					// 由於採全異步發射，不可複用 Pointer 避免 Data Race
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

					// Fire and Forget (無鎖異步)
					router.EmulateGatewayFlow(1, uid, reqID, env)
				}
			}
		}(i)
	}

	// 4. 等待時限到期，並切斷水管
	wg.Wait()
	log.Println("Traffic generation duration completed. Waiting for in-flight responses to drain...")

	// 確保所有異步打出去的 Envelope 都有收到 Server 的回覆 (或 Timeout)
	drainStart := time.Now()
	for {
		sent := router.reqSent.Load()
		handled := router.respRecv.Load() + router.errCount.Load()
		if handled >= sent {
			break
		}
		if time.Since(drainStart) > 15*time.Second {
			log.Printf("Drain timeout! Abandoning %d in-flight requests...", sent-handled)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 優雅關閉所有長連線，避免殘留
	router.StopAll()

	log.Printf("===== Load Test Finished =====")
	log.Printf("Total Sent Envelopes: %d", router.reqSent.Load())
	log.Printf("Total Success Responses: %d", router.respRecv.Load())
	log.Printf("Total Errors: %d", router.errCount.Load())
	log.Printf("Error Breakdown: %s", router.GetErrorBreakdown())
}
