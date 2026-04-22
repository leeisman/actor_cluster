# Persistence Module Specification

## 1. 模組定位與職責 (Module Boundary)

`pkg/persistence` 是極薄的 persistence adapter package。
它只負責建立 Cassandra session，並執行上層傳入的 CQL statement / query。

它不定義業務 event、不知道 wallet schema、不做 `(tenant_id, uid)` partition guard、不做 version / TxID / balance 規則。
這些全部屬於 `internal/node` 的 business Actor handler 或 node-local store adapter。

**職責內 (In Scope)：**
- 建立 / 關閉 Cassandra session。
- 執行 caller-provided CQL batch。
- 執行 caller-provided CQL query。
- 回傳底層錯誤並保留 wrapped error。

**職責外 (Out of Scope)：**
- 不定義 `WalletEvent`。
- 不知道 `wallet_events` table。
- 不知道 TenantID / UID / TxID / Balance / Version。
- 不做 cross-partition validation。
- 不做 business idempotency。
- 不做 rehydration。
- 不決定 retry / fail-fast / actor rebuild policy。

---

## 2. 核心 API

```go
type BatchMode int

const (
    LoggedBatch BatchMode = iota
    UnloggedBatch
)

type Statement struct {
    CQL        string
    Args       []any
    Idempotent bool
}

type Scanner interface {
    Scan(dest ...any) bool
}

type Store interface {
    ExecuteBatch(ctx context.Context, mode BatchMode, statements []Statement) error
    Query(ctx context.Context, cql string, args []any, scan func(Scanner) error) error
    Close()
}
```

`Statement.CQL` 與 `Args` 由上層提供。
`pkg/persistence` 不解析、不理解、不驗證業務欄位。

---

## 3. Cassandra Adapter

```go
type CassandraStore struct {
    session *gocql.Session
}

func NewCassandraStore(hosts []string, keyspace string) (*CassandraStore, error)
func (s *CassandraStore) ExecuteBatch(ctx context.Context, mode BatchMode, statements []Statement) error
func (s *CassandraStore) Query(ctx context.Context, cql string, args []any, scan func(Scanner) error) error
func (s *CassandraStore) Close()
```

`CassandraStore` 的唯一策略：
- `LoggedBatch` / `UnloggedBatch` 映射到 gocql batch type。
- `Statement.Idempotent` 原樣傳給 gocql。
- empty batch 回 `ErrEmptyBatch`。
- query scanner callback 由上層負責 scan 成自己的業務 struct。

---

## 4. 與 Internal Node 的分工

Wallet 相關操作應位於 `internal/node`：

```text
internal/node
  -> WalletEvent
  -> WalletEventStore
  -> wallet_events CQL
  -> cross-partition guard
  -> duplicate version guard
  -> rehydration / TxID cache / balance semantics

pkg/persistence
  -> ExecuteBatch
  -> Query
  -> Close
```

`cmd/node` 的 DI：

```text
cmd/node
  -> persistence.NewCassandraStore(hosts, keyspace)
  -> node.NewNode(store, resolver, resolvedIP)
  -> remote.NewServer(appNode, cfg)   // cfg 含 remote.ServerConfig，main 內 MaxBatchSize: 5000（與 04_remote_spec 對齊）
  -> pb.RegisterActorServiceServer(grpcServer, remoteServer)
```

---

## 5. Memory & Performance Rules

- `pkg/persistence` 不使用 reflection。
- `ExecuteBatch` 預先接收上層組好的 `[]Statement`，不做 schema mapping。
- 上層若要避免 allocation，應在 `internal/node` 的業務 store adapter 內管理 statement slice capacity。
- `Query` 不回傳 `[]map[string]any` 這類重型結構；它只提供 scanner callback。

---

## 6. 避坑紀錄 (Pitfalls & Workarounds)

- **不要把業務 event 放在 `pkg/persistence`**：`WalletEvent`、balance、TxID、version 都是業務語義，應在 `internal/node`。
- **不要讓 persistence 做 partition guard**：跨 partition batch 是否非法取決於上層 schema；`pkg/persistence` 只執行 statement。
- **不要讓 persistence 決定 rehydration**：載入最近 N 筆、重建狀態、TxID cache 都是 Actor handler 的策略。
- **不要在 persistence 裡寫固定 table CQL**：`wallet_events` 等 CQL 屬於 node business store adapter。
