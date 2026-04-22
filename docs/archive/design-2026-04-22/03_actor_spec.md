# Actor Runtime Specification

## 1. 模組定位與職責 (Module Boundary)

`pkg/actor` 是極薄的 Actor runtime primitive。
它只負責一件事：**把多 producer 送進來的 message 排成單一序列，交給 callback 在單一 Actor goroutine 中執行，並把 callback result emit 到注入的 result sink。**

核心語意：

```text
actor.Send(env)           // *pb.RemoteEnvelope
  -> mailbox enqueue
  -> actor event loop dequeue
  -> handler.Handle(env)
  -> resultSink.Emit(result)
```

**職責內 (In Scope)：**
- MPSC mailbox：多 producer、單 consumer。
- Actor event loop：保證同一 Actor 的 message callback 永遠單 goroutine、依序執行。
- Callback invocation：收到 message 後呼叫注入的 handler。
- Result emission：callback 回傳 result 後，Actor runtime 呼叫注入的 `ResultSink.Emit`。
- Lifecycle primitive：Start / Stop / IsAlive / context cancellation。
- Envelope / mailbox 物件重用，降低 hot path allocation。

**職責外 (Out of Scope)：**
- 不知道 gRPC transport / gateway 協定（import `pkg/remote/pb` 僅作為純 struct 資料定義，不含網路邏輯）。
- 不做 ownership、routing、slot、Node-to-Node forwarding。
- 不做 op_code validation、payload decode、業務規則。
- 不做 persistence、Cassandra、event sourcing。
- 不做 rehydration。
- 不做 TxID idempotency cache。
- 不做 response batching。
- 不定義 wallet / bet / rollback 等任何業務語義。

上述職責全部屬於上層 `internal/node` 或其內部業務 Actor handler。

---

## 2. 核心 API

### 2.1 Message

`pkg/actor` 直接使用 `*pb.RemoteEnvelope` 作為 Actor runtime 唯一排隊與傳遞的資料，**不再定義獨立的 `actor.Message` struct**。

`pb.RemoteEnvelope` 欄位語意：
- `TenantId` / `Uid`：Actor identity 與 result attribution。Node runtime 已用它們找到 Actor，result emit 時仍需這兩個欄位做可觀測性與歸因。
- `RequestId`：transport correlation key，async result sink 模型下 result 必須帶回才能讓 Node runtime 對應到 gateway request。
- `TxId`：業務冪等鍵，Actor runtime 不解讀，業務 handler 自行做 idempotency。
- `OpCode`：業務操作碼，Actor runtime 不解讀，業務 handler 自行 dispatch。
- `Payload`：業務 payload，Actor runtime 不 decode。
- `DeadlineUnixMs`：可選的軟性截止時間，Node runtime 可用於 fast reject。

**為什麼不用 `actor.Message`**：`actor.Message` 與 `pb.RemoteEnvelope` 欄位完全重複，在 `HandleBatch` 中多一次 struct copy，且欄位命名風格不一致（`TenantID` vs `TenantId`）。直接傳遞 `*pb.RemoteEnvelope` 消除整個轉換層，zero copy。

`pkg/actor` import `pkg/remote/pb`（純 struct 定義，無網路邏輯，可接受的依賴）。

### 2.2 ResultSink

```go
type ResultSink interface {
    Emit(result *pb.RemoteResult)
}
```

`ResultSink` 是 Actor runtime 與 Node runtime 的唯一 async result 出口，在 `New(handler, sink)` 時注入 Actor。

`pkg/actor` 直接使用 `*pb.RemoteResult` 作為 result 型別，省去 `actor.Result` 中間轉換層。
`pb` package 只包含純 struct 定義，無網路邏輯，可接受被 runtime primitive 依賴。

實作原則：
- 初版可由 `internal/node` 提供 result router / aggregator sink，內部把 result 丟到 node aggregator channel。
- `Emit` 不得丟失 result。
- 若 sink channel 滿，應阻塞形成 backpressure，而不是 silent drop。
- `Emit` 不應做 heavy work；response batching / remote send 都在 `internal/node` aggregator 內做。

`completeResult` 輔助函數在 `loop()` 內呼叫，負責將 `*pb.RemoteEnvelope` 的 identity 欄位自動回填進 `*pb.RemoteResult`（TenantId、Uid、TxId、RequestId、OpCode），handler 只需設定 Success / ErrorCode / Payload。

### 2.3 Handler Callback

```go
type Handler interface {
    Handle(env *pb.RemoteEnvelope) *pb.RemoteResult
}
```

`Handler` 由上層注入。
Actor runtime 不知道 handler 裡面是 wallet、bet、persistence、cache、idempotency 還是任何其他邏輯。

Invariant：
- 一個 input `*pb.RemoteEnvelope` 必須產生一個 `*pb.RemoteResult`。
- `Handler.Handle` 不應另開 goroutine 延後 emit；result 由 Actor runtime 統一 emit。
- 若業務需要 fire-and-forget，仍應回傳 success / accepted 類 result，避免 gateway request 永遠 pending。

### 2.4 Actor

```go
type Actor struct {
    handler    Handler
    sink       ResultSink
    shutdownFn func()       // 可選：Actor goroutine 退出前的 hook
    status     atomic.Int32
}

func New(handler Handler, sink ResultSink) *Actor
func (a *Actor) Start(ctx context.Context)
func (a *Actor) Send(env *pb.RemoteEnvelope) bool
func (a *Actor) SendEnvelope(env *Envelope) bool
func (a *Actor) SetShutdownFunc(fn func())
func (a *Actor) IsAlive() bool
func (a *Actor) IsDead() bool
func (a *Actor) Stop()
func (a *Actor) Drain() []*pb.RemoteEnvelope
```

`Send` / `SendEnvelope` 的語意：
- 成功 enqueue 回 `true`。
- Actor 已 stopping / dead 時回 `false`。
- `Send` 不執行業務邏輯，只投遞 envelope。

`IsDead()` vs `IsAlive()`：
- `IsAlive()` 在 statusInit / statusRunning 時為 true。
- `IsDead()` 只在 statusDead（goroutine 完全退出）時為 true。
- `Stop()` 設 statusStopping，此時 `IsAlive()` 為 false，但 `IsDead()` 尚未為 true（goroutine 仍在執行最後一筆 Handle）。

`Drain()` 的語意：
- **必須在 `IsDead()` 返回 true 後才能呼叫**，否則有 data race。
- 取出 mailbox 中尚未被 Actor goroutine 處理的剩餘 `*pb.RemoteEnvelope`。
- 主要用於 `forceKillLocalActor`：Actor 被強制驅逐後，把信箱殘留 envelope 抽出並立即回傳 WRONG_NODE 錯誤，讓 Gateway 毫秒內重試，而非等 timeout。

---

## 3. Mailbox 設計

### 3.1 MPSC Queue

Actor mailbox 使用 MPSC：

```text
many producers
  -> actor.Send(env)          // *pb.RemoteEnvelope
  -> MPSC mailbox
  -> one actor event loop
  -> handler.Handle(env)
  -> sink.Emit(result)
```

理由：
- Node runtime / remote stream worker 可能並行送 message。
- 同一 Actor 必須單 consumer 保序。
- Go channel 在多 producer 高壓下會有 mutex contention 與 scheduler overhead。

### 3.2 Dummy Node Invariant

內部 queue 可採 intrusive linked-list + dummy node：

```text
head -> dummy
tail -> dummy
```

Producer hot path：

```go
prev := tail.Swap(env)
prev.next.Store(env)
```

Consumer hot path：

```go
head := a.head.Load()
next := head.next.Load()
```

若 `next == nil` 且 `head != tail`，代表 producer 已 swap tail 但尚未接上 `next`，consumer 可短暫 `runtime.Gosched()`。

### 3.3 Wake Signal

可使用 1-buffered `wake chan struct{}` 作 park / unpark signal：

```go
select {
case wake <- struct{}{}:
default:
}
```

此 channel 不是 mailbox，只是喚醒信號。
核心 message queue 仍是 MPSC。

---

## 4. Event Loop

Actor event loop 的唯一工作：

```text
for {
  qnode := dequeue()
  if qnode == nil:
      if statusStopping:           // 信箱排空後才退出（graceful drain）
          shutdownFn()
          statusDead; return
      select { ctx.Done / wake }   // 等新訊息或關機信號
      continue

  env := qnode.Msg               // *pb.RemoteEnvelope
  result := handler.Handle(env)
  if result != nil:
      completeResult(result, env)  // 自動回填 TenantId/Uid/TxId/RequestId/OpCode
      sink.Emit(result)
  qnode.Msg = nil                // 釋放 pb.RemoteEnvelope 引用，允許 GC
}
```

**停止語意（Graceful Drain）**：
- `Stop()` 設 `statusStopping` 並送 wake 信號。
- loop 不在每次迭代頂部檢查 stopping；而是在信箱排空（`dequeue()` 回 nil）時才檢查並退出。
- 這保證 `Stop()` 被呼叫時信箱中剩餘的 envelope 仍會被完整處理，不會遺失。

重要約束：
- `handler.Handle` 同一時間只會有一個 goroutine 執行。
- Actor runtime 不 catch / retry / persist 業務結果。
- Actor runtime 不理解 result 語意。
- Actor runtime 不自行產生 business error code。
- 每個 envelope 最多 emit 一個 result；一進多出不屬於初版 runtime contract。

若 handler 需要 persistence、idempotency、state machine，全部在 handler 內自行處理。

---

## 5. Lifecycle

### 5.1 Start

`Start(ctx)` 啟動單一 Actor goroutine。
禁止每筆 message 開 goroutine。

### 5.2 Stop

已實作 Graceful Drain：
- `Stop()` 設 `statusStopping` 並喚醒 loop，**不立即退出**。
- loop 在信箱排空後才退出，確保 in-flight envelope 不遺失。
- `ctx.Done()` 取消時立即退出（不排空信箱），適合緊急撤銷場景。

若 Actor 停止後 `Send` 被呼叫，必須回 `false`，由 `internal/node` 轉成 failed result 或 goto retry 重建 Actor。

### 5.3 Idle Reclaim

Actor runtime 不內建 idle timeout。

原因：
- actor runtime 只負責 message serialization。
- actor 是否該回收，取決於 node registry / supervisor 對 UID ownership、memory pressure、rehydration 成本的策略。
- 若把 idle timeout 放進 actor runtime，runtime 會開始承擔 policy，邊界會變模糊。

因此 idle reclaim 應由 `internal/node` 或上層 supervisor 透過 `Stop()` / context cancel 控制。

---

## 6. Memory & Performance Rules

- `Send` hot path 不使用 `defer`。
- event loop hot path 不使用 `defer`。
- 不為每筆 message 建 goroutine。
- 不為每筆 message 建 reply channel。
- 若使用 Envelope pool，回收前必須將 `Msg` 指標設為 `nil`（清空 `pb.RemoteEnvelope` 引用）並清空 `next`，避免 GC 保留已處理的 envelope。
- Actor runtime 不 decode payload。
- Actor runtime 只 import `pkg/remote/pb`（純 struct，無網路邏輯），不 import gRPC / grpc-go。
- Actor runtime 不做 reflection。
- `Sink.Emit` 可能阻塞以形成 backpressure，但不應做 remote send 或重型聚合。

---

## 7. 與 Internal Node 的整合

`internal/node` 的真實 ActorDispatcher 應負責：

```text
*pb.RemoteEnvelope
  -> validation / ownership in internal/node
  -> find actor by (TenantId, Uid)
  -> actor.Send(env)           // 直接傳遞 *pb.RemoteEnvelope，無中間轉換
  -> actor handler returns *pb.RemoteResult
  -> actor runtime calls completeResult (auto-fills identity fields from env)
  -> actor emits result through Sink
  -> node stream aggregator receives result
  -> aggregate into pb.BatchResponse
  -> remote.BatchSender.SendBatchResponse
```

Actor runtime 不知道：
- `(TenantID, UID)` 怎麼路由
- `request_id` 怎麼轉成 `pb.RemoteResult`
- gateway 怎麼回客戶端
- response 何時 flush

### 7.1 Sink Scope

初版建議 actor 建立時注入 node-level result sink：

```text
one internal/node registry
  -> create actor.New(handler, resultSink)
  -> actor emits every result to resultSink
  -> resultSink routes / aggregates by RequestID or node-maintained correlation metadata
```

`*pb.RemoteEnvelope` 不帶 callback / sink。
這可以讓 hot path 更單純，也避免每筆 envelope 都攜帶一個 interface callback。

若未來同一 actor 需要服務多條 remote stream，routing 應由 `internal/node` 的 result sink / result router 處理，而不是把 callback 放回每筆 envelope。

---

## 8. Testing Strategy

必須使用 table-driven tests 覆蓋：

1. `Send`：
   - running actor 可 enqueue message
   - stopped actor reject message
2. ordering：
   - single producer 順序
   - multiple producers 最終每筆皆被處理
3. callback：
   - handler 收到 message
   - handler 依序執行，不並行
4. result sink：
   - handler result 會被 emit
   - 每筆 message emit 一筆 result
   - sink 阻塞時 Actor event loop 形成 backpressure
   - `New` 不接受 nil sink
5. lifecycle：
   - context cancel 退出
   - Stop 後 reject 新 message
6. memory hygiene：
   - envelope recycle 清空 `Msg` 指標（`nil`）與 `next`，避免 GC retention

---

## 9. Trade-off 分析

### 9.1 Actor Runtime vs Business Actor

若 `pkg/actor` 管 persistence、rehydration、TxID cache，它會變成業務框架，邊界會膨脹。
本設計把 `pkg/actor` 壓回 runtime primitive：只保證單 Actor 序列化執行 callback，並把 result emit 到 sink。
業務 Actor 是 `internal/node` 裡的 handler，不是 `pkg/actor` 本身。

### 9.2 Async Result Sink vs Per-message Reply Channel

`ReplyTo chan *pb.RemoteResult` 模型簡單，但每筆 request 都需要一個 callback channel 或 channel 復用策略，也會讓 dispatch worker 大量阻塞等待。

`ResultSink.Emit` 模型優勢：
- 不需要 per-message reply channel。
- Actor 處理完即 emit result，Node aggregator 可以獨立按時間 / 數量 flush。
- 更貼近 gateway -> actor -> remote response 的 batching 模型。
- 允許 Node runtime 用 per-stream aggregator channel 管理 response lifecycle。

取捨：
- `*pb.RemoteEnvelope` 必須攜帶 `RequestId`，讓 node result sink 可以做 correlation。
- Sink 滿時 Actor event loop 會被 backpressure 阻塞。
- 必須保證一個 message 產生一個 result，避免 gateway pending。

### 9.3 Public `Send(env)` vs Exposed Envelope

標準 API 為 `actor.Send(*pb.RemoteEnvelope)`，由 runtime 內部從 pool 取出 `Envelope` wrapper 並儲存指標，caller 無需感知 MPSC 內部結構。

若效能 benchmark 證明 pool 操作成為瓶頸，進階 API 可繞過：

```go
qnode := actor.GetEnvelope()
qnode.Msg = env
actor.SendEnvelope(qnode)
```

一般 caller 應使用 `Send(env)`。`SendEnvelope` 屬於進階/低層 API，caller 需自行保證 ownership 轉移語意。

---

## 10. Build Sequence

1. 移除 `pkg/actor` 對 persistence store 的直接依賴。
2. 移除 `Behavior[T]` 的 event sourcing contract，改為 `Handler` callback。
3. 新增 `ResultSink`（Emit 直接使用 `*pb.RemoteResult`，省去 `actor.Result` 中間層）。
4. ~~將 `Message` 改成攜帶 `TenantID`...~~ **已完成並進一步優化**：移除 `actor.Message`，`Send` / `Handle` 直接使用 `*pb.RemoteEnvelope`，消除 struct copy 轉換層。
5. 將 public API 收斂為 `New(handler, sink)`、`Start(ctx)`、`Send(env)`、`IsAlive()`、`Stop()`。
6. Event loop 改成 `handler.Handle(*pb.RemoteEnvelope) -> sink.Emit(*pb.RemoteResult)`。
7. 保留或重建 MPSC mailbox。
8. 不在 `pkg/actor` 實作 idle timeout；idle reclaim 交給 `internal/node` supervisor。
9. 更新 `pkg/actor` tests 為 callback / ordering / sink / lifecycle。
10. 後續在 `internal/node` 實作真正業務 Actor handler：persistence、idempotency、狀態機、payload decode。

---

## 11. 避坑紀錄 (Pitfalls & Workarounds)

- **不要讓 `pkg/actor` 管 persistence**：持久化是業務 handler 的策略，不是 runtime primitive。
- **不要讓 `pkg/actor` 管 TxID cache**：idempotency 屬於業務 / Node runtime。
- **不要讓 `pkg/actor` import gRPC / grpc-go**：transport layer 不可污染 runtime。`pkg/remote/pb` 只含純 struct，是可接受的依賴。
- **不要在 Actor runtime 內 decode payload**：payload 是業務格式。
- **不要在 hot path 使用 `defer`**。
- **不要為每筆 message 建 goroutine**。
- **不要為每筆 message 建 reply channel**：result 透過 sink 非同步回流。
- **不要把 `ResultSink` / callback 放進 `*pb.RemoteEnvelope`**：hot path 應只承載資料；result 出口在 `New(handler, sink)` 時固定注入 Actor，routing 交給 `internal/node` 的 sink / router。
- **不要 export `HandlerFunc` 便利 adapter**：`pkg/actor` 的 public API 應維持最小面積；測試若需要 function adapter，放在 test helper，不進 runtime contract。
- **不要讓 Actor runtime 負責 response batching**：batching 是 `internal/node` 的責任。
- **不要讓 Actor runtime 負責 idle reclaim**：actor 存活策略由 `internal/node` registry / supervisor 控制。
- **不要讓 sink silent drop result**：gateway request 會 pending；必須阻塞、回錯或由 Node runtime 明確處理。
