package node

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/frankieli/actor_cluster/pkg/actor"
	"github.com/frankieli/actor_cluster/pkg/persistence"
	"github.com/frankieli/actor_cluster/pkg/remote"
	pb "github.com/frankieli/actor_cluster/pkg/remote/pb"
)

const txCacheSize = 1000
const snapshotThreshold = 1000

// BusinessActor 封裝 pkg/actor 的底層 MPSC 引擎，並持有完整的錢包業務狀態。
//
// 狀態欄位的並發安全：
//   - balance、version、eventCount、txCache、txRing、txCursor
//     只在 actor.Actor 分配的單一 goroutine 內讀寫，無需任何鎖。
//   - lastActive 由 passivationLoop 跨 goroutine 讀取，因此使用 atomic.Int64。
type BusinessActor struct {
	core *actor.Actor

	tenantID int32
	uid      int64

	balance    int64
	version    int64 // 與 wallet_events.version 同步的單調遞增序號
	eventCount int

	txCache  map[string]struct{}
	txRing   []string
	txCursor int

	store persistence.Store

	lastActive atomic.Int64
}

func newBusinessActor(tenantID int32, uid int64, store persistence.Store, sink actor.ResultSink) *BusinessActor {
	b := &BusinessActor{
		tenantID: tenantID,
		uid:      uid,
		txCache:  make(map[string]struct{}, txCacheSize),
		txRing:   make([]string, txCacheSize),
		store:    store,
	}
	b.lastActive.Store(time.Now().UnixNano())
	b.core = actor.New(b, sink)
	return b
}

// rehydrate 從 Cassandra 重建 Actor 記憶體狀態（兩段式）。
//
//  1. 讀 wallet_snapshots → snapshotBalance + lastVersion
//  2. 讀 wallet_events WHERE version > lastVersion ORDER BY version ASC
//     → 驗證 version 連續遞增；若發現跳號（空洞），立即返回錯誤防止餘額計算出錯
//     → 累加 delta + 填充 txCache
func (b *BusinessActor) rehydrate(ctx context.Context) error {
	var snapshotBalance int64
	var lastVersion int64

	snapCQL := `SELECT balance, last_version FROM wallet_snapshots WHERE tenant_id = ? AND uid = ?`
	if err := b.store.Query(ctx, snapCQL, []any{b.tenantID, b.uid}, func(s persistence.Scanner) error {
		for s.Scan(&snapshotBalance, &lastVersion) {
		}
		return nil
	}); err != nil {
		return fmt.Errorf("rehydrate: load snapshot failed: %w", err)
	}

	balance := snapshotBalance
	prevVersion := lastVersion
	eventCQL := `SELECT version, tx_id, delta_amount FROM wallet_events WHERE tenant_id = ? AND uid = ? AND version > ? ORDER BY version ASC`
	if err := b.store.Query(ctx, eventCQL, []any{b.tenantID, b.uid, lastVersion}, func(s persistence.Scanner) error {
		var version int64
		var txID string
		var deltaAmount int64
		for s.Scan(&version, &txID, &deltaAmount) {
			if version != prevVersion+1 {
				return fmt.Errorf("version gap: expected %d got %d (data loss or out-of-order write)",
					prevVersion+1, version)
			}
			prevVersion = version
			balance += deltaAmount
			b.seedTxID(txID)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("rehydrate: load events failed: %w", err)
	}

	b.balance = balance
	b.version = prevVersion
	return nil
}

// Handle 實作 actor.Handler，在 Actor 的單一 goroutine 內執行業務邏輯。
//
// 流程：防重放 → 記憶體守門 → 盲寫（帶版本號）→ 更新記憶體狀態 → 觸發快照
// version 在 Actor goroutine 內單調遞增，無需原子操作。
//
// 所有 return 路徑都必須帶完整的 identity 欄位（TenantId/Uid/TxId/RequestId/OpCode），
// 因為 gateway 以非同步方式接收結果，需靠這些欄位才能定位回對應的 socket/請求。
// 不可依賴 pkg/actor.completeResult 作為唯一補填手段（測試直接呼叫 Handle 時不經過它）。
func (b *BusinessActor) Handle(env *pb.RemoteEnvelope) *pb.RemoteResult {
	now := time.Now()
	b.lastActive.Store(now.UnixNano())

	// base 持有所有 identity 欄位，各 return 路徑在此基礎上填充業務欄位即可。
	base := &pb.RemoteResult{
		TenantId:  b.tenantID,
		Uid:       b.uid,
		TxId:      env.TxId,
		RequestId: env.RequestId,
		OpCode:    env.OpCode,
	}

	if _, exists := b.txCache[env.TxId]; exists {
		// 冪等：交易已提交，只確認 Success，不回傳 balance。
		// txCache 僅存「是否存在」，無法重建原始 balance（那時可能只有 50，現在是 200），
		// 回傳當前 balance 會讓 gateway 誤認為這是「這筆 tx 結算後的餘額」。
		// gateway 若需要最新 balance 應走獨立的 read path。
		base.Success = true
		return base
	}

	if len(env.Payload) < 8 {
		base.Success = false
		base.ErrorCode = remote.ErrInvalidPayload
		base.ErrorMsg = "payload must be 8 bytes (int64 big-endian)"
		return base
	}
	amount := int64(binary.BigEndian.Uint64(env.Payload))

	newBalance := b.balance + amount
	if newBalance < 0 {
		base.Success = false
		base.ErrorCode = remote.ErrInsufficientFunds
		base.ErrorMsg = "balance cannot be negative"
		return base
	}

	nextVersion := b.version + 1
	stmt := persistence.Statement{
		// created_at 保留作為稽核欄位（非 PRIMARY KEY），version 為定址主鍵。
		CQL:        `INSERT INTO wallet_events (tenant_id, uid, version, created_at, tx_id, delta_amount, payload) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		Args:       []any{b.tenantID, b.uid, nextVersion, now, env.TxId, amount, env.Payload},
		Idempotent: false,
	}
	if err := b.store.ExecuteBatch(context.Background(), persistence.UnloggedBatch, []persistence.Statement{stmt}); err != nil {
		base.Success = false
		base.ErrorCode = remote.ErrPersistenceFailed
		base.ErrorMsg = err.Error()
		return base
	}

	b.balance = newBalance
	b.version = nextVersion
	b.addTxID(env.TxId)

	b.eventCount++
	if b.eventCount >= snapshotThreshold {
		b.takeSnapshotAsync()
		b.eventCount = 0
	}

	base.Success = true
	base.Payload = b.encodeBalance()
	return base
}

// takeSnapshotSync 同步快照，保留供手動維護指令使用（不作為 shutdownFn 自動觸發）。
func (b *BusinessActor) takeSnapshotSync() {
	if b.eventCount == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stmt := persistence.Statement{
		CQL:        `INSERT INTO wallet_snapshots (tenant_id, uid, balance, last_version) VALUES (?, ?, ?, ?)`,
		Args:       []any{b.tenantID, b.uid, b.balance, b.version},
		Idempotent: true,
	}
	_ = b.store.ExecuteBatch(ctx, persistence.UnloggedBatch, []persistence.Statement{stmt})
}

// takeSnapshotAsync fire-and-forget 非同步快照，每 snapshotThreshold 筆觸發一次。
// 複製 local 變數後啟動新 goroutine，Data Race 安全。
func (b *BusinessActor) takeSnapshotAsync() {
	balance, version, tenantID, uid, store :=
		b.balance, b.version, b.tenantID, b.uid, b.store
	go func() {
		stmt := persistence.Statement{
			CQL:        `INSERT INTO wallet_snapshots (tenant_id, uid, balance, last_version) VALUES (?, ?, ?, ?)`,
			Args:       []any{tenantID, uid, balance, version},
			Idempotent: true,
		}
		_ = store.ExecuteBatch(context.Background(), persistence.UnloggedBatch, []persistence.Statement{stmt})
	}()
}

func (b *BusinessActor) addTxID(txID string) {
	if old := b.txRing[b.txCursor]; old != "" {
		delete(b.txCache, old)
	}
	b.txCache[txID] = struct{}{}
	b.txRing[b.txCursor] = txID
	b.txCursor = (b.txCursor + 1) % txCacheSize
}

func (b *BusinessActor) seedTxID(txID string) {
	if _, exists := b.txCache[txID]; exists {
		return
	}
	b.txCache[txID] = struct{}{}
	b.txRing[b.txCursor] = txID
	b.txCursor = (b.txCursor + 1) % txCacheSize
}

func (b *BusinessActor) encodeBalance() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(b.balance))
	return buf
}
