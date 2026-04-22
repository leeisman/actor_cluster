# Discovery & Topology Resolver Specification

本文為 **`pkg/discovery` 的單一規格**：含 etcd 拓樸註冊/Watch，以及 (TenantID, UID)→`host:port` 的槽位、建表、查表**完整**契約（實作於 `table.go`、`etcd_resolver.go`）。`cmd/node` 與 `cmd/client` 的 discovery 使用對照見 **§14**。

## 1. 模組定位與系統邊界

`pkg/discovery` 是 Cluster 尋址層的唯一 primitive，負責將 `(TenantID, UID)` route key 穩定映射為目標 Node 的 `host:port`。**實際 slot 與預建表**由 `EtcdResolver` 內之 `atomic.Pointer[Table]` 持有；從 etcd 得到 sorted addrs 後建表並 `tab.Store(…)`，`GetNodeIP` 則 `GetNodeIP(tab.Load(), …)`；**不可**在其它套件重複實作建表迴圈或 `SlotOf`。

### 1.1 架構演進說明

本模組採用**去中心化的本地演算法路由 (Decentralized Algorithmic Routing)**。

捨棄了先前「etcd 中央維護 slotmap」的方案（兩個 key：`/nodes` JSON + `/slotmap` 1024 bytes），改為：
- etcd 只存活躍 Node 的 IP（Ephemeral Key）
- 每個 Node / Gateway 在本地記憶體中透過統一演算法，自行算出 `[1024]string` routing table

優勢：
- 消除 slotmap key 的維運成本（少量節點時 1024 個 key 大量重複）
- 節點上下線時 etcd 自動刪除 ephemeral key，不需要額外的清除機制
- 所有機器輸入一致（字典序排序的 IP 列表）→ 輸出必然一致，無腦裂風險

### 1.2 職責邊界

- 不做 node-to-node forwarding。
- 不承擔 actor lifecycle、passivation、rehydration。
- 不驗證業務 payload、opcode、deadline。
- 不在熱點查表路徑上執行任何網路 I/O 或 Heap Allocation。

## 2. etcd Schema

```text
Key:   /actor_cluster/nodes/{host:port}
Value: ""  (空，僅 Key 存在即代表節點存活)

特性:  Ephemeral Key，綁定 Lease。Node 當機時 etcd 自動刪除。
```

範例：
```text
/actor_cluster/nodes/10.0.0.1:9000  -> ""
/actor_cluster/nodes/10.0.0.2:9000  -> ""
/actor_cluster/nodes/10.0.0.3:9000  -> ""
```

## 3. 核心介面

```go
package discovery

import "context"

// TopologyResolver 定義熱點查詢合約。
// GetNodeIP 必須在納秒級別完成，無 Mutex、無 defer、無網路 I/O、無 Heap Alloc。
type TopologyResolver interface {
    GetNodeIP(tenantID int32, uid int64) (string, error)
    Watch(ctx context.Context) error
    Close() error
}
```

### 3.1 `GetNodeIP`
- 輸入 `(TenantID, UID)`，兩者共同構成 ownership key。
- 熱點路徑與同套件之 **`GetNodeIP(*Table, …)`** 一致；實作上即 `tab.Load` 讀出 `*Table` 再查。語意見 **§6.3、§9**。

### 3.2 `Watch`
- 啟動時先做一次 `GET {prefix}` prefix 查詢，拉取所有存活節點 IP。
- 對 IP slice 做字典序排序（**關鍵！確保所有機器輸入一致**）。
- `tab.Store(BuildRoutingTable(sortedIPs, slotOwner))` 換表（CoW）。
- 啟動背景 goroutine 監聽 `{prefix}` 的 PUT/DELETE 事件，任一觸發就全量重抓快照。
- Watch channel 關閉或錯誤時，sleep/backoff 後自動重建 watch，直到 `ctx.Done()`。

### 3.3 `Close`
- 只釋放 etcd client 等外部資源。
- Watch goroutine 生命週期由 `ctx` 控制，不在 Close 內處理。

## 4. 節點註冊 (EtcdRegistry)

`EtcdRegistry` 負責將本機 Node IP 以 Ephemeral Key 的形式維持在 etcd 中。

```go
// Register 完成首次 Grant + Put + KeepAlive 後立即返回。
// 背景 goroutine maintainRegistration 持續維持租約，斷線自動重建。
func (r *EtcdRegistry) Register(ctx context.Context, ip string) error
```

### 4.1 Key 拼接

```go
key := nodePrefix + ip
// e.g., "/actor_cluster/nodes/" + "10.0.0.1:9000"
//     = "/actor_cluster/nodes/10.0.0.1:9000"
```

### 4.2 Prefix 正規化

```go
"/actor_cluster/nodes"  -> "/actor_cluster/nodes/"
"/actor_cluster/nodes/" -> "/actor_cluster/nodes/"
""                      -> "/"
```

必須由 Registry / Resolver 內部正規化，不可依賴呼叫端記憶。

### 4.3 KeepAlive 重建流程

```
establishRegistration:
  loop:
    1. Grant lease (leaseTTL = 5s)
    2. Put key with lease
    3. KeepAlive
    成功 -> 返回 keepAliveCh

maintainRegistration:
  loop:
    consumeKeepAlive (等待 ch 關閉)
    sleepWithContext(200ms)
    重新 establishRegistration
```

## 5. Route Key 與 Slot 演算法

> **§5–§6** 為槽位、建表之規範性定義，與 `pkg/discovery` 實作一致。

### 5.1 Route Key 型別

- `TenantID`: `int32`
- `UID`: `int64`

禁止改成 `string`，避免 hash、編碼轉換與 GC 壓力進入熱點路徑。

### 5.2 Slot Count

```go
const PartitionSlots = 1024
const slotMask       = PartitionSlots - 1
```

`1024 = 2^10`，可用位元運算 `& 1023` 取代 `% 1024`。

### 5.2.1 `Table`

```go
type Table [PartitionSlots]string
```

- 索引即 **Slot ID**，值為該 slot 負責節點的 **`host:port`** 字串。
- 使用固定長度陣列（非 slice），讀路徑預測性與間接存取成本優於 slice header。
- 以 `RoutingTable` 型別別名指向同一型別（見 **§7**）。

### 5.3 `SlotOf` 函式

```go
func SlotOf(tenantID int32, uid int64) int {
    x := uint64(uint32(tenantID))<<32 ^ uint64(uid)
    x ^= x >> 33
    x *= 0xff51afd7ed558ccd
    x ^= x >> 33
    x *= 0xc4ceb9fe1a85ec53
    x ^= x >> 33
    return int(x & slotMask)
}
```

約束：
- Deterministic：所有 Gateway / Node 算出同一結果。
- 必須同時納入 `TenantID` 與 `UID`，避免不同 tenant 的相同 UID 永遠碰撞。
- 禁止依賴 runtime-randomized hash seed。

## 6. 本地建表演算法

### 6.1 `SlotOwnerFunc` 抽象

```go
// SlotOwnerFunc 決定 slot 應由哪個 node 擁有。
// 獨立抽出此型別，讓未來可無縫換成 Consistent Hashing Ring。
type SlotOwnerFunc func(sortedIPs []string, slot int) string

// ModuloSlotOwner 是目前的預設實作（本套件；len==0 時回傳 ""，不除零）。
// 輸入的 sortedIPs 必須已字典序排序。
func ModuloSlotOwner(sortedIPs []string, slot int) string
```

（語意同下節。）

### 6.2 `BuildRoutingTable`（原 buildRoutingTable）

實作位於本套件 `table.go`：

```go
func BuildRoutingTable(sortedNodeAddrs []string, owner SlotOwnerFunc) *Table
```

約束：
- 只依賴輸入參數，不做任何 I/O，不修改外部狀態。
- `sortedNodeAddrs` 為空時，回傳全空字串表（所有 slot 為 `""`），`GetNodeIP` 會回傳 `ErrSlotUnassigned` / `ErrNoOwner`（見 **§6.3、§10**）。
- 單元測試可完全脫離真實 etcd。

### 6.3 純表 `GetNodeIP`（`table.go`）

```go
func GetNodeIP(table *Table, tenantID int32, uid int64) (string, error)
```

1. `table == nil` → `ErrTableNotInitialized`（對應 `ErrRoutingTableNotInitialized`）。
2. `slot := SlotOf(tenantID, uid)`。
3. `ip := table[slot]`；`ip == ""` → `ErrNoOwner`（對應 `ErrSlotUnassigned`）。
4. 否則回傳 `(ip, nil)`。

`EtcdResolver` 的 `GetNodeIP` 僅做 `GetNodeIP(r.tab.Load(), …)`。熱點路徑：無 `mutex`、無 `defer`、成功路徑無額外 heap alloc（見 **§9**）。

## 7. In-memory Layout

`RoutingTable` 即 `Table` 的型別別名；執行時表體由 **`EtcdResolver` 的 `tab atomic.Pointer[Table]`** 持有（見 **§5.2.1、§6.3、§9**）。

```go
// 欄位 tab：sync/atomic.Pointer[Table]
type EtcdResolver struct {
    tab        atomic.Pointer[Table]
    cli        *clientv3.Client
    nodePrefix string
    slotOwner  SlotOwnerFunc
}
```

### 7.1 為何用 `[1024]string` 固定陣列

- 陣列長度編譯期已知，CPU 預取效率更高。
- 避免 slice header 的額外 pointer 間接存取。
- Copy-on-Write：整份陣列重建後 `tab.Store(新 *Table)`，舊讀者持有的快照不可變。

### 7.2 為何用 `atomic.Pointer[Table]`

- `GetNodeIP` 對表做一次 `Load` 再查，無鎖競爭。
- 寫入端使用 copy-on-rebuild，不直接修改 live table。

## 8. Watch / Refresh 策略

### 8.1 全量重抓，不做增量 patch

收到任何 topology event 後，執行一次完整 `GET {prefix}` 快照：

```
1. GET /actor_cluster/nodes/ (WithPrefix)
2. 提取 IP → 字典序排序
3. `tab.Store(BuildRoutingTable(sortedIPs, slotOwner))`
```

不做增量 patch 的原因：
- 字典序排序必須在完整 IP 列表上執行，才能保證所有機器的建表一致性。
- 全量重抓讓建表邏輯更簡單，無狀態依賴，且 topology 變更是低頻事件。

### 8.2 Failure Semantics

| 情境 | 行為 |
|---|---|
| 初始 `GET` 失敗 | `Watch()` 直接返回 error（尚無 routing table 可用） |
| Watch channel 關閉 | sleep(100ms) → 重建 watch |
| Watch response 帶錯誤 | sleep(100ms) → 重建 watch |
| `refreshSnapshot` 失敗 | 保留 `tab` 內舊表，不覆寫，等待下一次事件 |
| `ctx.Done()` | 唯一合法的 watch loop 終止條件 |

### 8.3 壓測 / Gateway 送出前自檢

若要在**送出 RPC 前**手動驗證目標與叢集語意一致：

1. 取得與執中邏輯相同的**已排序**節點列表（與本節 `sortedNodeIPsFromKVs` 相同排序鍵）。
2. `table := BuildRoutingTable(sortedNodeAddrs, nil)`。
3. `GetNodeIP(table, tenantID, uid)` 與即將撥的 gRPC `host:port` 比對；不一致則應**刷新拓樸**再送，勿改寫本套件之 `SlotOf`/建表。

## 9. 熱點讀路徑合約

`EtcdResolver.GetNodeIP` 即 **`GetNodeIP` 讀表**（同套件）：

```go
func (r *EtcdResolver) GetNodeIP(tenantID int32, uid int64) (string, error) {
    return GetNodeIP(r.tab.Load(), tenantID, uid)
}
```

**硬限制**（整條 `GetNodeIP` 路徑）：
- 無 Mutex Lock/RLock
- 無 `defer`（在熱路徑內）
- 無網路 I/O
- 無任何 Heap Allocation（包含 `fmt.Sprintf`、建立新 slice）

## 10. Error Surface

| Error | 語意 |
|---|---|
| `ErrRoutingTableNotInitialized` | Resolver 尚未完成首次 snapshot 載入 |
| `ErrSlotUnassigned` | 目前無任何存活 Node，或 slot 對應 IP 為空 |

這些均屬 infrastructure 層錯誤，不應被包裝成業務錯誤碼。

## 11. Test Matrix

演算法層測試以 **`pkg/discovery` 內** `routing_test.go` 等為主；etcd/Watch 整合則同套件之 `etcd_resolver_test.go` 等（見下表）。

| # | 測試案例 |
|---|---|
| 1 | `SlotOf` 結果永遠落在 `[0, 1024)` |
| 2 | `SlotOf` 真的包含 `TenantID`（不同 TenantID + 相同 UID → 不同 slot）|
| 3 | `ModuloSlotOwner` 對多節點的 slot 分配符合預期 |
| 4 | `BuildRoutingTable` 單節點：所有 slot 指向同一 IP |
| 5 | `BuildRoutingTable` 多節點：modulo 均分 |
| 6 | `BuildRoutingTable` 空節點列表：回傳全空表 |
| 7 | `sortedNodeIPsFromKVs` 正確排序與提取 IP |
| 8 | `hasNodeTopologyEvent` 正確識別 PUT / DELETE |
| 9 | `GetNodeIP` 路由正確 |
| 10 | Uninitialized table → `ErrRoutingTableNotInitialized` |
| 11 | Empty table → `ErrSlotUnassigned` |
| 12 | Copy-on-Write 不污染舊快照 |
| 13 | `normalizeNodePrefix` prefix 正規化正確 |

Integration test 另補：
- 真實 etcd 初始載入
- Watch event 觸發刷新
- Watch 中斷後可恢復
- Node 下線後 ephemeral key 自動刪除

## 12. Trade-offs

### 12.1 我們接受的成本
- Topology 更新時有整表 rebuild allocation（低頻事件，可接受）
- Watch 錯誤恢復期間短暫使用舊表（Cassandra version 保護確保最終一致）
- `RoutingTable [1024]string` 佔用固定記憶體

### 12.2 我們拒絕的成本
- 熱點路徑 mutex / channel / network I/O
- Runtime-randomized routing（跨進程不一致）
- 讀路徑超過一次 atomic load
- 增量 patch 導致建表不一致（字典序排序必須在完整列表上執行）

## 13. 避坑紀錄

### 13.1 IP 列表排序是分散式一致性的基石
所有 Node / Gateway 的輸入陣列必須字典序一致，後續 modulo / consistent hashing 才能算出相同的 owner。少了這個排序，整個去中心化路由就會各說各話。

### 13.2 Prefix 必須由 Resolver 內部正規化
呼叫端不應記住是否要帶尾斜線，否則 `/actor_cluster/topologynodes` 這類拼接錯誤難以 debug。

### 13.3 Watch Loop 不可靜默死亡
背景同步一旦停掉，整個 Cluster 會在無告警下持續使用舊 topology，比顯式失敗更危險。必須 backoff 重建，直到 `ctx.Done()`。

### 13.4 `BuildRoutingTable` 必須維持純函式
不做 I/O、不修改外部狀態，讓單元測試可完全脫離 etcd，快速覆蓋大部分 correctness（見 **§6.2**）。

### 13.5 舊架構的「中央 slotmap」已廢除
不再於 etcd 維護 `{prefix}/slotmap`（1024 bytes）與 `{prefix}/nodes`（JSON）兩個 key。現在 etcd 只存 Node 存活狀態（ephemeral key），routing 邏輯完全在本地記憶體中執行。

### 13.6 槽位演算法以 `pkg/discovery` 內實作為唯一實作
**其它套件**不得重複實作 `SlotOf` / 建表迴圈，僅能經本套件之 `BuildRoutingTable` + `tab.Store` / `GetNodeIP(tab.Load(), …)` 途徑（即 `TopologyResolver` 所暴露語意；見 **§5–§6、§9**）。

### 13.7 自檢與線上邏輯同輸入
壓測或工具若自行 `BuildRoutingTable`+`GetNodeIP` 驗證目標，**節點字串清單必須與 etcd 快照採用相同排序規則**（**§2、§8.1、§8.3**），否則自檢通過但實際仍可能打錯節點或反之。

## 14. 執行檔邊界（`cmd/node` / `cmd/client`）

兩執行檔的 discovery 路徑必須一致，避免 client 與節點各算各的：

| 項目 | `cmd/node` | `cmd/client` |
|------|------------|----------------|
| `import pkg/discovery` | 是，僅此套件 | 是，僅此套件 |
| `EtcdRegistry` | 是（註冊本機 `host:port`） | 否 |
| 建立 resolver | `NewEtcdResolver(etcd, nodePrefix)` | 同上、同預設 `node-prefix` |
| 啟動拓樸 | `Watch(ctx)`（與主流程共用 signal ctx） | 同上 |
| 查表 | 經 `TopologyResolver` → `GetNodeIP` 注入 `internal/node` / gRPC 前之所有權 | 經 `Router.EmulateGatewayFlow` 等 → `GetNodeIP` |
| 禁止行為 | 在 `main` 自算 `SlotOf` 或重複建表 | 同左 |

變更槽位/建表語意只改 **`pkg/discovery`** 一處；兩執行檔皆不得另開路徑分叉。
