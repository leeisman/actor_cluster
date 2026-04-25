package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/frankieli/actor_cluster/internal/node"
	"github.com/frankieli/actor_cluster/pkg/actor"
	"github.com/frankieli/actor_cluster/pkg/discovery" // NewEtcdRegistry + NewEtcdResolver + Watch；查表=TopologyResolver.GetNodeIP（與 cmd/client 同契約，見 docs/design/01_discovery_spec.md §14）
	"github.com/frankieli/actor_cluster/pkg/persistence"
	"github.com/frankieli/actor_cluster/pkg/remote"
	"github.com/frankieli/actor_cluster/pkg/remote/pb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

// resolveMyIP 決定本節點對外廣播的 host:port，優先順序：
//  1. --ip 明確指定（CI/CD、手動測試時使用）
//  2. POD_IP 環境變數（K8s Downward API 自動注入）
//  3. UDP 探測法（VM / 本機開發，讓 OS routing table 告訴我們主要網卡 IP）
//
// 回傳值格式永遠是 host:port。
// --ip 若只傳裸 IP（不含 port），自動補上 --port 指定的 port。
// UDP 探測法取 localAddr.IP.String()（純 IP），不可用 localAddr.String()，
// 因後者包含隨機的本地 UDP port，與 gRPC 監聽 port 無關。
// net.JoinHostPort 正確處理 IPv6（如 [::1]:50051），不可用 fmt.Sprintf 拼接。
//
// ⚠️  架構決策：嚴禁以 `:0` 隨機 Port 啟動（見 ai/memory-bank/02_tech_decision.md §3.1）
// IP:Port 是本節點寫入 etcd 的唯一識別標記。隨機 Port 在重啟後會被視為新節點加入，
// 觸發 1024 Slot 重分配（Re-sharding）→ 大量 Actor Rehydrate → Cassandra 查詢風暴。
func resolveMyIP(flagIP, port string) (string, error) {
	cleanPort := strings.TrimPrefix(port, ":")

	if flagIP != "" {
		// 若已包含 port（如 10.0.0.1:50051）直接使用；否則補上 cleanPort。
		if _, _, err := net.SplitHostPort(flagIP); err != nil {
			flagIP = net.JoinHostPort(flagIP, cleanPort)
		}
		log.Printf("Using explicitly provided --ip: %s", flagIP)
		return flagIP, nil
	}

	if podIP := os.Getenv("POD_IP"); podIP != "" {
		resolved := net.JoinHostPort(podIP, cleanPort)
		log.Printf("Auto-detected IP from POD_IP env: %s", resolved)
		return resolved, nil
	}

	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", fmt.Errorf("UDP probe failed: %w", err)
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP.String()
	resolved := net.JoinHostPort(ip, cleanPort)
	log.Printf("Auto-detected IP from network interface: %s", resolved)
	return resolved, nil
}

func main() {
	port := flag.String("port", ":50051", "gRPC server listen address (e.g. :50051)")
	metricsAddr := flag.String("metrics", ":9090", "HTTP metrics listen address (e.g. :9090)")
	myIP := flag.String("ip", "", "advertised host:port for etcd registration; auto-detected if empty")
	cassHosts := flag.String("cassandra", "127.0.0.1", "comma-separated Cassandra hosts")
	keyspace := flag.String("keyspace", "wallet", "Cassandra keyspace")
	etcdEndpoints := flag.String("etcd", "127.0.0.1:2379", "comma-separated etcd endpoints")
	nodePrefix := flag.String("node-prefix", "/actor_cluster/nodes", "etcd key prefix for node registration")
	flag.Parse()

	log.Println("Starting Actor Node...")

	// ── 0. 根 Context（信號感知）────────────────────────────────────────────
	//
	// signal.NotifyContext 讓 SIGINT/SIGTERM 直接取消 ctx。
	// registry.Register 與 etcdResolver.Watch 的背景 goroutine 持有此 ctx，
	// 收到信號後會自動停止 keepalive / watchLoop，無需額外通知。
	// stop() 釋放 signal.NotifyContext 的內部資源，必須在 main 結束前呼叫。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── 1. 解析本節點對外 IP ────────────────────────────────────────────────
	resolvedIP, err := resolveMyIP(*myIP, *port)
	if err != nil {
		panic(err)
	}

	// ── 2. Persistence（Cassandra）─────────────────────────────────────────
	hosts := strings.Split(*cassHosts, ",")
	store, err := persistence.NewCassandraStore(hosts, *keyspace)
	if err != nil {
		panic(err)
	}
	defer store.Close()

	// ── 3. etcd Client ─────────────────────────────────────────────────────
	endpoints := strings.Split(*etcdEndpoints, ",")
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		panic(err)
	}
	defer etcdCli.Close()

	// ── 4. Service Discovery：Registry（ephemeral 自我註冊）+ Resolver ──────
	//
	// 槽位/建表只存在於 pkg/discovery；本二進位不應自算 SlotOf 或重複建表。
	// internal/node 所有權檢查與 resolver 使用同一條查表路徑（GetNodeIP）。
	//
	// 傳入 ctx（信號感知）：收到 SIGINT/SIGTERM 後，keepalive goroutine 與
	// watchLoop 會透過 ctx.Done() 感知並優雅退出，不需要額外 channel 通知。
	registry := discovery.NewEtcdRegistry(etcdCli, *nodePrefix)
	if err := registry.Register(ctx, resolvedIP); err != nil {
		panic(err)
	}
	log.Printf("Registered node in etcd: %s%s", *nodePrefix, resolvedIP)

	etcdResolver := discovery.NewEtcdResolver(etcdCli, *nodePrefix)
	if err := etcdResolver.Watch(ctx); err != nil {
		panic(err)
	}
	log.Println("Topology resolver started.")

	// ── 5. Actor Node Runtime ───────────────────────────────────────────────
	appNode := node.NewNode(store, etcdResolver, resolvedIP)

	// ── 6. gRPC Server ──────────────────────────────────────────────────────
	lis, err := net.Listen("tcp", *port)
	if err != nil {
		panic(err)
	}

	grpcServer := grpc.NewServer()
	remoteServer := remote.NewServer(appNode, remote.ServerConfig{MaxBatchSize: 5000})
	pb.RegisterActorServiceServer(grpcServer, remoteServer)

	go func() {
		log.Printf("gRPC server listening on %s\n", *port)
		if err := grpcServer.Serve(lis); err != nil {
			panic(err)
		}
	}()

	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("/metrics", handleMetrics)
	metricsServer := &http.Server{
		Addr:              *metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("metrics server listening on %s\n", *metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()

	// ── 7. Graceful Shutdown ─────────────────────────────────────────────────
	//
	// 關機順序（由上游往下游切斷，Pipeline 分段關閉）：
	//  1. ctx 取消 → keepalive goroutine、watchLoop 自動退出
	//  2. Deregister：主動刪除 etcd key，立即觸發其他節點的 watch 事件，
	//     加速路由表切換，避免在 lease TTL（5s）內繼續收到新流量。
	//  3. grpcServer.GracefulStop()：關閉 TCP 入口，等 in-flight RPC 完成。
	//  4. appNode.Stop()：排空 Actor mailbox，flush 最後一批結果給 Gateway。
	<-ctx.Done()
	stop() // 釋放 signal.NotifyContext 資源，避免重複信號觸發
	log.Println("Shutting down server gracefully...")

	if err := registry.Deregister(context.Background(), resolvedIP); err != nil {
		log.Printf("Deregister warning: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("metrics shutdown warning: %v", err)
	}

	grpcServer.GracefulStop()
	appNode.Stop()

	log.Println("Server stopped gracefully.")
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP node_actor_mailbox_pending Total number of envelopes currently waiting across all actor mailboxes within this node process.\n")
	fmt.Fprintf(w, "# TYPE node_actor_mailbox_pending gauge\n")
	fmt.Fprintf(w, "node_actor_mailbox_pending %d\n", actor.ProcessMailboxPending())
}
