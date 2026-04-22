# Spec: `internal/node` (Business Node Runtime)

## 1. 模組定位與職責

`internal/node` 是 Actor Cluster 的核心業務執行層，直接實作 `pkg/remote` 呼叫的 Handler，負責：

1. **接收與驗證**：接收 `pb.BatchRequest`，進行基礎 Envelope 驗證（TenantID / UID / TxID 非空）。
2. **所有權防護（Ownership Guard）**：呼叫 `discovery.TopologyResolver.GetNodeIP`，確認此 Envelope 應屬於本節點，若非 owner 立刻回傳 `WRONG_NODE` 並驅逐本地殘留 Actor。
3. **Actor 路由與生命週期**：256 Shard Map 管理 `*BusinessActor`。Actor 不存在時以兩段式 Rehydrate（快照 + Delta Events）重建狀態後上線。
4. **業務邏輯與冪等性**：Actor 單 goroutine 保護記憶體餘額，盲寫（Blind Write）只存 `delta_amount`，Ring Buffer TxID Cache 防重放。
5. **結果聚合**：`aggregatorLoop` 依時間（5ms）或數量（1000）門檻批次送回 Gateway。
6. **閒置回收（Passivation）**：每小時掃描，驅逐 24 小時未活躍的 Actor。
7. **快照管理（Snapshot）**：每 1000 筆事件非同步快照；**不**在退出時強制存快照（避免關機雪崩效應，詳見 4.5）。
8. **對帳補償（Reconciliation）**：`reconciliation.go` 骨架，每日掃描 Cassandra 重算真實餘額，對負餘額發補償交易。

---

## 2. DB Schema

### wallet_events（盲寫 Append-Only）
```sql
PRIMARY KEY ((tenant_id, uid), version)
CLUSTERING ORDER BY (version ASC)
-- 欄位：tenant_id INT, uid BIGINT, version BIGINT,
--        created_at TIMESTAMP,   -- 非 PK，僅作稽核用途
--        tx_id TEXT, delta_amount BIGINT, payload BLOB
-- 只存變動量 (delta_amount)，絕對不存餘額
-- version 由 Actor goroutine 在記憶體內單調遞增指派，無需分散式序號產生器
```

### wallet_snapshots（UPSERT，單行最新快照）
```sql
PRIMARY KEY ((tenant_id, uid))
-- 欄位：tenant_id INT, uid BIGINT, balance BIGINT, last_version BIGINT
```

---

## 3. 核心資料結構

```go
// ActorKey 作為 Shard Map 的唯一鍵值，並封裝 Shard 計算邏輯
type ActorKey struct {
    TenantID int32
    UID      int64
}

func (k ActorKey) shardIndex() uint64 {
    return uint64(k.TenantID^int32(k.UID)) % 256
}

// Node 是具體的 Actor 執行容器，不使用 Interface
type Node struct {
    shards [256]struct {
        sync.RWMutex
        actors map[ActorKey]*BusinessActor
    }
    store    persistence.Store
    resolver discovery.TopologyResolver  // O(1) 所有權查詢
    myIP     string                      // 本節點對外廣播的 host:port
    sender   remote.BatchSender          // 唯一 gRPC stream
    resultCh chan *pb.RemoteResult        // MPSC 結果佇列（Actor 直接寫入 pb，無中間層）
    done     chan struct{}
    wg       sync.WaitGroup
}

// BusinessActor 封裝 pkg/actor MPSC 引擎並持有完整業務狀態
// 所有欄位（除 lastActive）只在 Actor 單 goroutine 內讀寫，無需任何鎖
type BusinessActor struct {
    core       *actor.Actor
    tenantID   int32
    uid        int64
    balance    int64        // 記憶體中的最新餘額
    version    int64        // 與 wallet_events.version 同步的單調遞增序號
    eventCount int          // 自上次快照以來的成功事件數，達 1000 觸發快照
    txCache    map[string]struct{}
    txRing     []string     // Ring Buffer，淘汰最舊 TxID
    txCursor   int
    store      persistence.Store
    lastActive atomic.Int64 // 供 passivationLoop 跨 goroutine 讀取
}
```

---

## 4. 核心運算流程

### 4.1 HandleBatch（接收 → 所有權防護 → 分發）

```
for each env in BatchRequest.Envelopes:
  1. 基礎驗證（TenantID / UID / TxID 非空）
  2. O(1) 所有權防護：resolver.GetNodeIP(env.TenantId, env.Uid)
     → 若非本節點：forceKillLocalActor + Emit(WRONG_NODE) + continue
  3. RLock → 查 Shard Map
  4. 若不存在：Lock → double-check → newBusinessActor → rehydrate → Start → store map
  5. core.Send(env)                   // 直接傳入 *pb.RemoteEnvelope，無 actor.Message 中間轉換
     → 若 false（passivation Race）：Lock → delete stale ref → goto retry
```

### 4.2 Rehydrate（兩段式冷啟動）

```
Step 1：SELECT balance, last_version FROM wallet_snapshots
        → 無資料：snapshotBalance=0, lastVersion=0（查詢自動涵蓋全部歷史）
Step 2：SELECT version, tx_id, delta_amount FROM wallet_events
        WHERE version > last_version ORDER BY version ASC
        → 完整性檢查：驗證 version 連續遞增（prevVersion+1 == version）
          若發現跳號（空洞）立即返回錯誤，防止餘額計算出錯
        → balance = snapshotBalance + sum(delta_amounts)
        → 所有 tx_id → seedTxID（填充 txCache，不做淘汰）
```

> **為什麼改用 version 取代 created_at 定址**：
> Cassandra TIMESTAMP 精度為毫秒。在同一毫秒內（Batch 寫入或快速連續請求），
> 多筆事件的 `created_at` 可能相同，導致 `WHERE created_at > last_event_time`
> 漏讀同毫秒的其他事件。Version 是 Actor goroutine 在記憶體內指派的單調遞增整數，
> 精度無損、唯一確定，`WHERE version > last_version` 保證精準無漏。

### 4.3 Handle（盲寫業務邏輯）

```
1. now := time.Now()；lastActive.Store(now.UnixNano())
2. 防重放：txCache 命中 → Success=true, Payload=空（CQRS：txCache 無法重建原始結算 balance，
   回傳當前 balance 會產生語義歧義；需要 balance 走獨立 read path）
3. 解 payload → amount（Big-Endian int64）
4. 記憶體守門：balance + amount >= 0，否則 INSUFFICIENT_FUNDS
5. nextVersion := version + 1
6. 盲寫：INSERT INTO wallet_events (tenant_id, uid, version, created_at, tx_id, delta_amount, payload)
         VALUES (?, ?, nextVersion, now, txID, amount, payload)  // 無 LWT/CAS
7. balance += amount；version = nextVersion
8. addTxID(txID)（Ring Buffer 淘汰 + 插入）
9. eventCount++；若 >= 1000 → takeSnapshotAsync()；eventCount = 0
```

### 4.4 TxID Cache 輔助方法

| 方法 | 用途 | 是否淘汰舊 key |
|------|------|--------------|
| `addTxID(txID)` | Handle 成功路徑（運行中替換） | ✅ 先淘汰再插入 |
| `seedTxID(txID)` | rehydrate 冷啟動填充 | ❌ 舊 key 本就不存在 |

### 4.5 快照策略

| 觸發時機 | 方法 | 同步/非同步 |
|---------|------|-----------|
| 每 1000 筆成功事件 | `takeSnapshotAsync()` | 非同步（fire-and-forget，不阻塞熱路徑）|

`takeSnapshotAsync` 複製 local 變數後啟動新 goroutine，Data Race 安全。快照失敗無所謂，rehydrate 從更早的快照 + 更多 Events 補齊。

**刻意不在 Actor 退出時強制寫快照（關機雪崩效應）**

「Actor 死前同步寫快照」看似讓下次 rehydrate 更快，但在有 10 萬個活躍 Actor 的節點上，
Node.Stop() 會在同一秒觸發 10 萬次並發 UPSERT wallet_snapshots，導致：

1. Cassandra CPU 瞬間飆高，影響整個叢集其他節點。
2. 本節點因等待 10 萬次網路 I/O，關機時間拖長，最終被 K8s 強制 SIGKILL 中斷。

正確選擇：**讓 Actor 安靜且快速地死去**。未快照的 500 筆 Delta 早已安全躺在
`wallet_events` 表裡，rehydrate 的計算成本留給接手的新節點分散承擔。
這才是高效的分散式負載平衡，而非在關機瞬間製造 I/O 風暴。

> `takeSnapshotSync` 方法保留於程式碼中，供未來特殊情境（例如單一 Actor 的手動維護
> 指令）使用，但不作為 shutdownFn 自動觸發。

### 4.6 forceKillLocalActor（腦裂清場 + 快速失敗）

```
1. Lock → delete from map → Unlock   （最小鎖範圍，Stop() 在 Lock 外）
2. bActor.core.Stop()                 （非阻塞，僅設 statusStopping）
3. background goroutine:
   spin until IsDead()                （等 Actor goroutine 完全退出）
   → Drain()                          （抽取信箱殘留訊息）
   → Emit WRONG_NODE × N             （Gateway 毫秒級收到錯誤，即時 Retry）
```

### 4.7 Graceful Shutdown（Pipeline 分段由上游往下游切斷）

```
gRPC TCP → HandleBatch → MPSC mailbox → [Actor goroutine] → resultCh → aggregatorLoop → gRPC stream
                                           ↓ 寫 Cassandra ↑
```

**關閉順序**：
1. `grpcServer.GracefulStop()`：關上游入口，等 in-flight HandleBatch 完成
2. 遍歷 256 Shard，`bActor.core.Stop()`：排空 mailbox、寫完 Cassandra、把最後 Result 推入 resultCh
3. `close(n.done)`：Actor 全死，resultCh 不再有新資料，通知 aggregatorLoop 退出
4. `n.wg.Wait()`：aggregatorLoop 執行最後一次 flush，把「遺言」送回 Gateway

> **陷阱**：若先 `close(done)` 再停 Actor，高流量下 resultCh buffer 塞滿後
> Actor goroutine 永久阻塞在 Emit，Process 無聲無息 Hang 住，無任何 Error Log。

---

## 5. cmd/node 啟動流程

`EtcdResolver` 內之 **`GetNodeIP` 即同套件之 `GetNodeIP(tab.Load(), …)`**（見 `01_discovery_spec.md` §6.3、§9）。`cmd/node` 不額外 `import` 或自算 `SlotOf`；`internal/node` 與遠端 Client 路徑因此共用**同一**路由語意。與 **`cmd/node/main.go`** 註解及建構順序（Registry → Resolver.Watch → `node.NewNode(store, resolver, …)`）及 **`01_discovery_spec.md` §14** 一致。

```
resolveMyIP（--ip 優先 → POD_IP env → UDP 探測網卡）
  → CassandraStore
  → etcd Client
  → EtcdRegistry.Register(ctx, resolvedIP)   // ephemeral key，背景 keepalive；ctx 感知 SIGTERM
  → EtcdResolver.Watch(ctx)                  // 同步 snapshot + 背景 watchLoop；ctx 感知 SIGTERM；內部 tab.Store 建表
  → node.NewNode(store, resolver, resolvedIP)
  → grpcServer.Serve()
  → wait SIGINT/SIGTERM（signal.NotifyContext）
    → registry.Deregister()         // 主動刪 etcd key，立即觸發其他節點 watch，加速路由切換
    → grpcServer.GracefulStop()     // 關 TCP 入口，等 in-flight RPC 完成
    → appNode.Stop()                // 排空 Actor mailbox，flush 最後一批結果
```

---

## 6. 避坑紀錄（Pitfalls & Lessons Learned）

1. **`requests sync.Map` 是效能殺手**：Gateway 已用 slot 路由保證只有單一 gRPC Stream，Node 只需一個 `sender` 欄位，完全無需 Map 查找。

2. **Passivation RCU 兩階段掃描反而慢**：Shard 只有幾千個 Actor，兩次迭代 + slice allocation 的 GC 壓力遠超過單次 Lock 掃描。直接用 `Lock` 更快且零 Allocation。

3. **passivation Race Condition**：`Send()` 回傳 false 時，必須 `goto retry` 重建 Actor，而非遞迴（遞迴有堆疊開銷）。

4. **移除 `actor.Result` 與 `actor.Message` 中間層**：
   - `actor.Result` 與 `pb.RemoteResult` 欄位幾乎完全重複；直接讓 `Handler.Handle` 回傳 `*pb.RemoteResult`，消除整個轉換層。
   - `actor.Message` 與 `pb.RemoteEnvelope` 欄位完全重複；移除 `actor.Message`，`Actor.Send` / `Handler.Handle` 直接使用 `*pb.RemoteEnvelope`，`HandleBatch` 省去整個 struct copy 步驟。
   - `pkg/actor` 依賴 `pkg/remote/pb`（純 struct 定義，無網路邏輯，可接受）。

5. **快照必須用 Go 端時間戳**：Cassandra `now()` 無法讓 Go 端知道確切時間，導致快照的 `last_event_time` 與事件查詢基準錯位，漏讀 delta events。

6. **不在關機時強制存快照（關機雪崩效應）**：10 萬個 Actor 同時 `takeSnapshotSync` = 10 萬次並發 Cassandra UPSERT，節點被 K8s SIGKILL 收場。Delta events 已安全落地，rehydrate 成本由接手節點分散承擔。`SetShutdownFunc` 保留作為可選 hook，預設不使用。

7. **forceKill 不能在 Lock 內呼叫 Stop()**：Stop() 等 Actor 執行完最後一筆 Handle（含 Cassandra write），若在鎖內呼叫會導致整個 Shard 被 I/O 延遲卡死。

8. **Drain() 必須等 IsDead()**：Stop() 是非阻塞的，goroutine 仍在執行最後的 Handle。只有 `IsDead() == true`（statusDead 已設定）才能安全遍歷 MPSC 鏈結串列。
