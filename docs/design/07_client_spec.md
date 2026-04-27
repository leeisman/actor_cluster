# Client / Gateway Specification

本文件定義 `cmd/client` 的現行角色：它不是單純的 CLI 壓測器，而是**同一個 binary 下的雙模式入口**。

- `stress`：原本的高 TPS load generator
- `serve`：玩家入口用的 test web gateway server

`cmd/client` 仍然必須遵守 `pkg/discovery` 的單一真相邊界：**不可**在本模組內重寫 `SlotOf`、routing table 或 owner 演算法；所有目標節點解析一律透過 `TopologyResolver.GetNodeIP`。

## 1. 模組定位

`cmd/client` 的職責：

1. 透過 etcd watch 拿到當前叢集拓樸。
2. 依 `(tenant_id, uid)` 把請求送到正確的 node `host:port`。
3. 對每個目標 IP 維護單一 gRPC bidi stream，做批次聚合與結果回收。
4. 在 `stress` 模式中做 fire-and-forget 高吞吐發射。
5. 在 `serve` 模式中做同步 request/response bridge，作為真實玩家 HTTP 入口。
6. 在 `serve` 模式中提供 Cassandra 直連餘額查詢，作為 debug / 對帳工具。

不屬於 `cmd/client` 的事：

- 不承擔 actor 業務狀態。
- 不實作 node-to-node forwarding。
- 不自己計算 slot / owner。
- 不把 Cassandra 直連查詢當成正式 read model。

## 2. 執行模式

### 2.1 `stress`

用途：保留原本的 CLI 壓測模式。

啟動範例：

```bash
go run ./cmd/client stress -tps 50000 -concurrency 5000 -duration 30s
```

行為：

- 啟動 etcd resolver。
- 建立 `Router`。
- 依 `tps / concurrency` 限速產生大量 `RemoteEnvelope`。
- 每筆請求只記錄 latency 起點，不建立等待 channel。
- 背景由 `ReceiverLoop` 收到 `BatchResponse` 後更新 success / error / latency metrics。
- 目前 `stress` 另支援：
  - `-drain-window`
  - `-uid-min`
  - `-uid-max`

其中：
- `drain-window`：停止送流量後，最多再等待多久讓 in-flight 請求排空。
- `uid-min` / `uid-max`：控制壓測隨機 `uid` 範圍，用來模擬熱點 workload 與較分散 workload。

### 2.2 `serve`

用途：啟動 test web gateway server，作為玩家入口與 Ops console。

啟動範例：

```bash
go run ./cmd/client serve \
  -http :8080 \
  -etcd 127.0.0.1:2379 \
  -cassandra 127.0.0.1:9042 \
  -keyspace wallet
```

行為：

- 啟動 etcd resolver。
- 建立與 `stress` 共用的 `Router` / `NodeStreamer` transport pipeline。
- 建立 Cassandra session，供 debug balance query 使用。
- 啟動內嵌 HTML dashboard 與 HTTP API。

## 3. 核心結構

### 3.1 Router

`Router` 是 `cmd/client` 的核心 transport 調度器。

```go
type Router struct {
    resolver   discovery.TopologyResolver
    batchSize  int
    flushDelay time.Duration

    streamers sync.Map  // map[string]*NodeStreamer
    mu        sync.Mutex // 保護 getStreamer 的 double-check lock

    // Metrics
    reqSent        atomic.Uint64
    respRecv       atomic.Uint64
    errCount       atomic.Uint64
    latencyNanos   atomic.Uint64
    latencySamples atomic.Uint64

    errBreakdown sync.Map // map[string]*atomic.Uint64；error code 細分統計
}
```

`batchSize` 來自共用 CLI flag：

```bash
-batch=2000
```

在 `make load-test` 中可透過：

```bash
BATCH=2000
```

覆寫，用來調整每次送往單一 node 的最大批次大小。

`flushDelay` 同樣來自共用 CLI flag：

```bash
-flush-delay=5ms
```

在 `make load-test` 中可透過：

```bash
FLUSH_DELAY=5ms
```

覆寫，用來控制未滿 batch 最多等待多久就強制 flush。

它提供兩種送法：

- `EmulateGatewayFlow(...)`
  用於 `stress` 模式，只做 fire-and-forget。
- `ExecuteAndWait(env, timeout)`
  用於 `serve` 模式，建立同步等待橋接。

### 3.2 NodeStreamer

對每個目標 IP 維護一條長壽命 gRPC stream。

```go
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

    failOnce sync.Once
    dead     atomic.Bool
}
```

每個 `NodeStreamer` 固定只有兩條常駐 goroutine：

1. `flusherLoop`
2. `receiverLoop`

這是 `cmd/client` 高併發下的重要設計原則：**goroutine 數量與目標 node 數量成正比，而不是與 request 數量成正比**。

另外，`NodeStreamer` 對 transport failure 採 **fail-fast + replace** 策略：

1. 若 `stream.Send()` 或 `stream.Recv()` 發生 error，當前 `NodeStreamer` 會將自己標記為 `dead`
2. 這條 stream 底下尚未完成的 pending callback 會直接以 transport error 收斂
3. 舊 `grpcConn` / `stream` / `context` 會被關閉
4. `Router` 會將該 `NodeStreamer` 從目標 IP 對應表中移除
5. 後續新 request 再打到同一個 target node 時，由 `Router` 建立新的 `NodeStreamer`

這代表：

- **新 request** 具備基本自癒能力
- **舊 in-flight request** 若 response path 已斷，則直接視為失敗，不在 transport 層偷偷重送

重試責任留給上層（例如 gateway / client），並依 `request_id` / `tx_id` 的去重語意決定是否安全重送。

### 3.3 Sharded Callback Map

`ShardedCallbackMap` 採 256 shards，key 為全域唯一 `request_id`。

value 為：

```go
type callbackEntry struct {
    startedAt time.Time
    respCh    chan *pb.RemoteResult
}
```

兩種模式共用同一張表：

- `stress`：`respCh == nil`
- `serve`：`respCh != nil`

這讓同一條 transport 同時支撐高吞吐壓測與同步 HTTP 玩家請求，而不需要分裂兩套 client stack。

另外，`ShardedCallbackMap` 目前維護一個 **process-local aggregate counter**：

```text
CallbackPending
```

語意是：
- 當前 `cmd/client` process 內
- 所有 streamer callback map 尚未完成的 pending request 總數

需要注意的是，`CallbackPending` 的語意**比** `InFlight` 更寬：

- `CallbackPending`
  - 只要 request 已經建立 callback entry，就會計入
  - 因此同時包含：
    - 已成功送上 gRPC stream、正在等待 response 的 request
    - 尚未真正送出、但已卡在 `NodeStreamer.envelopeCh` / batch pipeline 內等待 flush 的 request
- `InFlight`
  - 目前僅以 `reqSent - (respRecv + errCount)` 推導
  - 只代表「已成功送上 wire，但尚未被 success / error 收斂」的 request

因此在高壓或 transport 失敗場景下，兩者**不保證永遠相等**。

用途：
- 在 `stress` / `load-test` log 中觀察 client 端 callback backlog 是否持續累積
- 協助區分「actor 端 backlog」與「client completion/bookkeeping backlog」

在 CLI summary 與 live log 中，這個值目前會以較明確的人類可讀名稱輸出為：

```text
PendingCallbacks(total)
Pending Callbacks (total)
```

## 4. 併發與資料流

### 4.1 路由與送出

所有請求路徑都必須：

1. 產生全域唯一 `request_id`
2. 呼叫 `resolver.GetNodeIP(tenantID, uid)`
3. 取得對應 `NodeStreamer`
4. 將 envelope 放入該 streamer 的 `envelopeCh`

嚴禁：

- 在 `cmd/client` 內自行計算 slot
- 在 `cmd/client` 內建立第二份 routing table

### 4.2 Fire-and-Forget Stress Path

`stress` 模式下：

1. 記錄 `startedAt`
2. `respCh` 保持 `nil`
3. `receiverLoop` 收到結果後只更新 metrics

這樣可避免每秒數萬筆 request 產生大量 channel allocation。

### 4.3 Sync HTTP Bridge Path

`serve` 模式下：

1. HTTP handler 建立 `respCh := make(chan *pb.RemoteResult, 1)`
2. 呼叫 `ExecuteAndWait`
3. `ReceiverLoop` 依 `request_id` 找回 `respCh`
4. 將對應 `RemoteResult` 回送給 handler
5. handler 再轉成 HTTP JSON response

這讓 transport 維持 async/batched，而對玩家呈現同步 request/response。

## 5. HTTP API

### 5.1 `POST /api/wallet/execute`

玩家真實請求入口。

Request:

```json
{
  "uid": 1,
  "amount": 100
}
```

流程：

1. 驗證 `uid > 0`
2. 產生全域唯一 `request_id`
3. 將 `amount` 轉為 Big-Endian `int64` payload
4. 建立 `RemoteEnvelope{OpCode=1}`
5. 呼叫 `ExecuteAndWait(..., 5s)`
6. 把 `RemoteResult` 映射成 HTTP response

Response 範例：

```json
{
  "success": true,
  "request_id": "281474976710657",
  "uid": 1,
  "amount": 100,
  "error_code": "",
  "error_msg": "",
  "balance": 1500
}
```

注意：
- `request_id` 雖然在系統內是 `uint64`，但對外 HTTP JSON 應以 **string** 回傳，避免前端 JavaScript 因超過 `Number` 安全整數範圍而失去精度。

狀態碼：

- `200`：成功
- `400`：`ERR_INVALID_PAYLOAD` / `ERR_INSUFFICIENT_FUNDS`
- `503`：`ERR_DISCOVERY` / `ERR_CONNECTION` / `ERR_TRANSPORT_CLOSED` / `ERR_STREAM_INTERRUPTED` / `ERR_WRONG_NODE`
- `504`：`ERR_DEADLINE_EXCEEDED`

注意：

- 這一層既然是玩家入口，就必須是同步等待結果的模式。
- timeout 只代表 gateway 等待逾時，不代表後端一定沒處理成功。

### 5.2 `GET /api/wallet/balance/:uid`

debug / 對帳用途，不走 actor write path。

流程：

1. `SELECT balance, last_version FROM wallet_snapshots WHERE tenant_id = ? AND uid = ?`
2. `SELECT version, delta_amount FROM wallet_events WHERE tenant_id = ? AND uid = ? AND version > ? ORDER BY version ASC`
3. 在 gateway 記憶體中累加 delta，並以**最後一筆 event 的 version** 更新最終版本號；若沒有增量 event，才退回 snapshot 的 `last_version`

Response 範例：

```json
{
  "uid": 1,
  "exact_balance": 1500,
  "last_version": 17
}
```

這條 API 是 Cassandra 單 partition query，不是全表掃描；但它仍然只是 debug 工具，不是正式 CQRS read model。

### 5.3 `POST /api/stress/start`

啟動背景壓測。

Request:

```json
{
  "tps_target": 50000,
  "concurrency": 5000,
  "duration_sec": 30
}
```

流程：

1. 檢查目前是否已有壓測在跑
2. 若無，建立背景 `StressEngine`
3. 立即回 `200`

### 5.4 `POST /api/stress/stop`

停止背景壓測。若沒有正在執行的壓測，回傳 idempotent 成功即可。

### 5.5 `GET /api/stress/status`

回傳目前壓測狀態與 metrics：

```json
{
  "is_running": true,
  "total_sent": 150000,
  "total_success": 149990,
  "total_errors": 10,
  "current_tps": 49950,
  "avg_latency_ms": 2.4,
  "error_breakdown": {
    "ERR_WRONG_NODE": 10
  }
}
```

注意：
- `total_sent` / `total_success` / `total_errors` 是累積值
- `current_tps` 是近一秒的 send TPS
- 它不等於「完整完成吞吐量」

### 5.6 `POST /api/stress/reset`

用途：清除 dashboard 上目前的累積 metrics，方便比較下一批測試。

這個 reset 的語意是：

- 不停止目前的 gRPC stream
- 不清除 Cassandra 資料
- 不重建 `Router`
- 只把 dashboard 顯示用的 metrics baseline 重設為「現在這一刻」

因此 reset 後重新歸零的是：

- `total_sent`
- `total_success`
- `total_errors`
- `avg_latency_ms`
- `error_breakdown`

而不是整個 transport runtime。

## 6. Web UI

`serve` 模式在 `/` 提供內嵌單檔 dashboard。

目前包含：

- Manual execute 表單
- Balance query 表單
- Stress start / stop 控制
- Clear metrics 按鈕
- Live metrics
- Event log

設計原則：

- 不引入前端 build pipeline
- 以單檔 HTML / CSS / JS 內嵌在 Go 內
- 偏 Ops console / debug dashboard 取向，而不是正式產品前台

## 7. 容錯與生命週期

### 7.1 Dead Streamer Eviction

若 `NodeStreamer` 發生 send / recv transport failure：

1. 標記 `dead = true`
2. 從 `Router.streamers` 中 evict
3. fail 掉該 streamer 的所有 pending callbacks
4. cancel 自己的 context

之後 `Router.getStreamer(ip)` 不得重用這個 dead streamer，而必須重建新連線。

這是為了避免某個 target IP 因一個暫時性 gRPC 異常，整場測試都黑洞化。

### 7.2 Graceful Shutdown

`stress` / `serve` 關閉時：

1. 停止新流量進入
2. `flusherLoop` 先 flush buffer
3. `stream.CloseSend()`
4. 等 `receiverLoop` 把剩餘回應收完
5. 關閉 gRPC connection

### 7.3 Timeout Cleanup

同步 `ExecuteAndWait` 若 timeout：

- 必須移除該 `request_id` 對應的 pending callback
- 不可留下 memory leak

### 7.4 Load-Test 結束語意

`stress` / `make load-test` 結束時，目前會額外輸出：

- `Config`
- `Generation Duration`
- `Drain Duration`
- `Total Completion Duration`
- `Offered TPS`
- `Completed TPS`
- `Callback Map Pending`
- `Drain Complete`
- `InFlight Unfinished`

定義：

- `Config`
  - 印出本輪實際使用的：
    - `tps`
    - `concurrency`
    - `batch`
    - `duration`
    - `drain_window`
    - `uid_range`
- `Generation Duration`
  - 真正送流量的時間（例如 30s）
- `Drain Duration`
  - 停止送流量後，等待 in-flight 排空所花的時間
- `Total Completion Duration`
  - `Generation Duration + Drain Duration`
- `Offered TPS`
  - `total_sent / generation_duration`
- `Completed TPS`
  - `total_success / total_completion_duration`

重要：

補充：

- `make load-test` 啟動的是 **K8s Job**，不是綁定在本地 terminal session 上的前景程序。
- 因此即使本地 terminal 離開，cluster 內的 load-generator 仍可能繼續執行。
- 為了明確停止壓測，Makefile 提供：
  - `make stop-load-test JOB_ID=<n>`
  - `make stop-all-load-tests`
- 當 `Drain Complete = true` 時，`Completed TPS` 可視為「這批 workload 被完整消化後的吞吐量」
- 當 `Drain Complete = false` 時，`Completed TPS` 只代表「在 drain window 內已完成部分的平均吞吐量」，**不是最終完整吞吐量**
- `InFlight Unfinished > 0` 時，表示仍有 request 未完成；這些未完成請求不會被算進 `total_success`

## 8. 工程規則與避坑

1. `request_id` 必須維持叢集全域唯一。
2. 不可在 `cmd/client` 內複製 discovery 數學。
3. `stress` 模式禁止每筆建立 response channel。
4. `serve` 的 execute 必須同步等待結果，因為這一層是玩家真實入口。
5. `GET /api/wallet/balance/:uid` 是 debug 工具，不代表正式 read side。
6. dead streamer 一定要能 evict 並重建，不可永久 cache 壞連線。
7. 所有 transport 錯誤字串應與 `pkg/remote/errors.go` 對齊。
