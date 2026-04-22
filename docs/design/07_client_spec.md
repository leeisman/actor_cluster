# Client (Load Generator) Specification

本文件定義 `cmd/client` (發射端與壓測負載產生器) 的高密度架構設計。嚴格遵守 `04_go_guidelines` 規範，拒絕無意義的 Goroutine 增生，並利用無鎖機制與批次 I/O 實現極限吞吐。

**與 `pkg/discovery` 內之槽位/查表**（完整契約見 `01_discovery_spec.md` §5–§6、§14）：Client **不**直接實作 `SlotOf` 或預建表。取得目標 `host:port` 的**唯一**入口為 `TopologyResolver.GetNodeIP`（`EtcdResolver` 內 `atomic` 持表 + 拓樸刷新），與 `cmd/node` / `internal/node` 所有權檢查使用同一條查表語意。

### 與 `cmd/client` 原始碼對照

| 步驟 | 實作位置 |
|------|----------|
| etcd + `NewEtcdResolver` + `Watch` | `main.go` |
| `TopologyResolver` 注入 `Router` | `NewRouter(resolver, …)` |
| 每請求解析目標節點 | `router.go` → `EmulateGatewayFlow` → `resolver.GetNodeIP` |
| **不**在 `main` 外額外 `import` 重複實作路由數學 | 僅用 `pkg/discovery`（`TopologyResolver`），與 `01_discovery_spec.md` §14 一致 |

## 1. 核心職責 (Core Responsibilities)
1. **Topology Awareness (拓樸感知)**: 直連 etcd，利用 `pkg/discovery.EtcdResolver`（`TopologyResolver` 介面）Watch 拓樸並以 **`GetNodeIP(tenantID, uid)`** 解析目標 `POD_IP:port`；槽位雜湊與 1024 欄位預建表之數學**與** `EtcdResolver` **同套件**實作，避免本程式與叢集內各節點公式漂移。
2. **Aggressive Batching (侵略性批次打包)**: 實體網路邊界必須打包。無法忍受 1 個 Request 浪費 1 次 TCP 系統呼叫，必須將大量單筆 `RemoteEnvelope` 根據目標 IP 進行聚合，打包為定時定量的 `BatchRequest`。
3. **Stream Multiplexing (全雙工流傳輸)**: 維護對每一台 NodeIP 的單一長連線 (gRPC Bidi-Stream，對應 Node 端的 `StreamMessages` API)，大幅降低 TCP 握手與 HTTP/2 Header overhead。
4. **Zero-Lock Metrics (無鎖數據採集)**: 壓測過程中的 TPS 與延遲統整，必須依賴 `sync/atomic` 或無鎖佇列，嚴禁使用 `sync.Mutex` 拖累吞吐。

## 2. 結構與核心介面 (Core Structures & Interfaces)

```go
// NodeStreamer 負責對 單一目標伺服器 (POD_IP) 維護一條完整的發送/接收流與其專屬的 Aggregation Buffer
type NodeStreamer struct {
    TargetIP   string
    grpcConn   *grpc.ClientConn
    stream     pb.ActorService_StreamMessagesClient
    
    // Batch Aggregation
    envelopeCh chan *pb.RemoteEnvelope // 有界 Channel，作為生產(發送器)與消費(Flusher)間的緩衝
    batchSize  int                     // Batch 上限門檻 (例如：1000)
    flushDelay time.Duration           // 最大 Flush 延遲 (例如：5ms)，時間到或塞滿即發送

    // Metrics
    reqSent    atomic.Uint64
    respRecv   atomic.Uint64
}

// ShardedCallbackMap：256 分片，每片一把 Mutex + map[reqID]chan（與實作一致）
// type ShardedCallbackMap 見 cmd/client/router.go

// Router 負責接管 TopologyResolver，進行全網觀測與派發
type Router struct {
    resolver    discovery.TopologyResolver
    batchSize   int
    flushDelay  time.Duration
    callbackMap *ShardedCallbackMap
    streamers   sync.Map // map[IP]*NodeStreamer
}
```

## 3. 演算法與併發控制 (Concurrency Strategy)

### 3.1 拓樸觀測與單 IP 雙 Goroutine 模型
為了避免 Gourtoutine 隨 Request 氾濫，Client 端實作 **1 個目標 IP Node 只需要長駐 2 個 Goroutine**：
1. **Sender Loop (Flusher)**: 負責監聽 `envelopeCh` 與 `time.Ticker`。當陣列長度達到 `batchSize` 或是 Ticker 觸發，立刻將陣列建立為 `BatchRequest`，執行 `stream.Send()`。
2. **Receiver Loop**: 背景阻擋在 `stream.Recv()`。一收到 `BatchResponse`（內含上千筆 `RemoteResult`），立刻迴圈並利用 `atomic.AddUint64` 增加成功數、解析 Histogram 取樣時間差。

### 3.2 極端記憶體利用 (Memory Allocation Trade-off)
- **避免動態分配 (Zero-Allocation)**: `BatchRequest` 內的 `Envelopes` slice 每次 flush 完畢後，不將其丟棄交給 GC。而是保留 Reference 替換或直接設定 `batch = batch[:0]` (但保留 Capacity)，確保底層陣列無限覆用。
- **Pool 取用**: 高頻產生的 `*pb.RemoteEnvelope` 本體，由全域唯讀資料池 (Read-only Seed) 複製，或統一透過 `sync.Pool` 借用，避免逃逸至 Heap 產生碎片。

### 3.3 Gateway 請求關聯與 Demultiplexing (ReqID Correlation)
如果 `cmd/client` 更貼近真實世界的「Gateway（長連線閘道器）」，它必須掛載數以萬計的真實客戶端 WebSocket 或 TCP Sockets，並將下游的 gRPC 回應精準對應回正確的 Socket 來源。為此，我們設計了針對高併發優化的非同步回調機制 (Correlation)：

1. **ReqID 配發**: 每個來自 Socket 的業務異動，都會被 Gateway 賦予一個全局唯一的 `ReqID`，寫入 `RemoteEnvelope`。
2. **Sharded Callback Map**: 為了避免單一 map 的鎖競爭，實作 **256 分片**（`reqID % 256`），每片為 **`sync.Mutex` + `map[uint64]chan *pb.RemoteResult`**（`cmd/client` 內之 `ShardedCallbackMap`），與以 `[256]*sync.RWMutex` 綁一張大 map 的寫法不同，但分片意圖相同。
3. **Dispatch & Callback**:
   ```go
   // 1. 產生 ReqID 與回調通道，並鎖入等待區
   reqID := generateReqID()
   respCh := make(chan *pb.RemoteResult, 1)
   router.callbackMap.Store(reqID, respCh) 
   
   // 2. 利用 (TenantID, UID) 經 GetNodeIP 算出目標 host:port（底層與 `cmd/node` 同 discovery 實作），丟給該 IP 的 Flusher 佇列
   targetIP, _ := router.resolver.GetNodeIP(tenantID, uid)
   router.getStreamer(targetIP).envelopeCh <- envelope
   
   // 3. 處理該 Socket 的 Goroutine 阻塞等待，直到 K8s 下游處理完畢
   result := <-respCh 
   writeToClientSocket(socket, result)
   ```
4. **Receiver 喚醒反解**: 當 `NodeStreamer.ReceiverLoop` 從 gRPC 收到整包 `BatchResponse`，會高速迴圈解開上千筆 Result，並對每個 `result.ReqID` 從等待區中取出 `chan` 進行 `ch <- result`，從而喚醒特定的 Socket Goroutine。這個機制不僅能模擬壓力，也正是正式 Gateway 機制該有的設計。

## 4. 容錯與重分配 (Error Handling & Re-sharding)

1. **`ERR_WRONG_NODE` 的捕捉**: 若 K8s Node Pod 掛掉、或新 Node 加入，etcd lease 失效期間 (5s 內) 會有不一致的空窗期。此段期間，Client 算出的 IP 可能錯誤，會收到 `RemoteResult.error_code == ERR_WRONG_NODE`（字串，見 `04_remote_spec.md` §7）。
2. **Backoff & Retry 機制**（可選、尚未於 `cmd/client` 全面實作）: 針對 `ERR_WRONG_NODE`，應在拓樸收斂後**重新**呼叫 `GetNodeIP`（新表仍由同套件 resolver 內之 `BuildRoutingTable`+`Store` 定義）再送，而非在 Client 內自算 slot。
3. **Graceful Shutdown**: Client 收到 SIGTERM 時，停止 Traffic Generator，`NodeStreamer.Sender` 執行強迫 flush，並呼叫 `stream.CloseSend()` 寫入 EOF。等待 `Receiver` 收到 K8s 遠端 Server 處裡完所有的 in-flight responses 送回後安全關閉，最後印出 Benchmark 報告。

## 5. 工程與部署整合 (K8s Job Integration)

作為本機或 K8s Job 啟動時，**以 `cmd/client` 的 command-line flag 為準**（`main.go` after `flag.Parse`），常見參數例如：
- `-tps=50000`：目標整體 TPS
- `-batch=1000`：每包上限
- 另有 `-concurrency`、`-duration`、`-etcd` 等

K8s Job 若要調參，通常寫在 **args** 或從 **env 注入腳本再拼成** 與上列等效之參數；以實際 `deploy` manifest 為準。
- 壓測 Job 本身無須暴露 K8s Port。它會從 etcd (例如 `etcd-client:2379`) 拉取拓樸，然後宛如水砲一般，從 K8s `10.244.x.x` 發送大量 L4 封包直接橫穿 CNI 轟炸目標 Actor Node。
- 結果僅輸出 Stdout，留由 EFK/Loki/CloudWatch 統整後續分析。

## 6. 避坑紀錄

1. **嚴禁在 `cmd/client` 內實作或複製 `SlotOf` / 建表**：歷史文檔曾誤寫 `resolver.SlotOf`；正確路徑僅有 **`GetNodeIP`**（**`pkg/discovery` 單一真相**，見 `01_discovery_spec.md` §14）。
2. **`ERR_DISCOVERY` / `ERR_WRONG_NODE`**（見 `04_remote_spec.md` §7.3、§7.2）：前者為 `GetNodeIP` 失敗（resolver 尚未就緒或無 owner）；後者為已送達節點但所有權已變，需刷新拓樸後再 `GetNodeIP`，**不可**在 client 內以本地公式「修正」目標而繞開 **`pkg/discovery` 內**之槽位/表實作。串流中斷時未完成之回調可為 `ERR_STREAM_INTERRUPTED`。
