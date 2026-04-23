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

    streamers sync.Map // map[string]*NodeStreamer

    reqSent        atomic.Uint64
    respRecv       atomic.Uint64
    errCount       atomic.Uint64
    latencyNanos   atomic.Uint64
    latencySamples atomic.Uint64
}
```

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
  "request_id": 281474976710657,
  "uid": 1,
  "amount": 100,
  "error_code": "",
  "error_msg": "",
  "balance": 1500
}
```

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
2. `SELECT delta_amount FROM wallet_events WHERE tenant_id = ? AND uid = ? AND version > ? ORDER BY version ASC`
3. 在 gateway 記憶體中累加 delta

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

## 6. Web UI

`serve` 模式在 `/` 提供內嵌單檔 dashboard。

目前包含：

- Manual execute 表單
- Balance query 表單
- Stress start / stop 控制
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

## 8. 工程規則與避坑

1. `request_id` 必須維持叢集全域唯一。
2. 不可在 `cmd/client` 內複製 discovery 數學。
3. `stress` 模式禁止每筆建立 response channel。
4. `serve` 的 execute 必須同步等待結果，因為這一層是玩家真實入口。
5. `GET /api/wallet/balance/:uid` 是 debug 工具，不代表正式 read side。
6. dead streamer 一定要能 evict 並重建，不可永久 cache 壞連線。
7. 所有 transport 錯誤字串應與 `pkg/remote/errors.go` 對齊。
