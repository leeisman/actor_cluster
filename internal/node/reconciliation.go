package node

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/frankieli/actor_cluster/pkg/persistence"
)

// 對帳模組使用 version 欄位進行全量重算，與 BusinessActor.rehydrate 保持一致。
// wallet_events schema:  PRIMARY KEY ((tenant_id, uid), version)
// wallet_snapshots schema: balance, last_version

// RunDailyReconciliation 是每日執行一次的背景對帳任務。
//
// ═══════════════════════════════════════════════════════════════
// 為什麼需要這個任務？
// ═══════════════════════════════════════════════════════════════
//
// 本系統採用 AP（高可用）策略，Actor 以記憶體單執行緒作為業務鎖，
// 拒絕了 Cassandra LWT/CAS 等強一致性手段。
//
// 在極端情況下（例如 etcd topology 更新延遲 + 高流量同時打同一個 UID），
// 同一個 Actor 可能短暫存活在兩個 Node 上：
//
//   Node A（舊路由）：收到 TxID=X，balance=100，扣款 50，blind write delta=-50 ✓
//   Node B（新路由）：同時收到 TxID=Y，也看到 balance=100，扣款 80，blind write delta=-80 ✓
//   最終 DB：100 - 50 - 80 = -30（負餘額！）
//
// 這個任務會定期掃描 Cassandra，從源頭重算每個 UID 的真實餘額。
// 若發現累加後餘額 < 0，觸發警報並發出補償交易。
//
// ═══════════════════════════════════════════════════════════════
// 補償交易策略（Compensating Transaction）
// ═══════════════════════════════════════════════════════════════
//
//  1. 寫入審計日誌：記錄 (tenant_id, uid, computed_balance, discrepancy) 供人工複核。
//  2. 發出正向補償 delta：若真實餘額 = -300，寫入 delta_amount = +300，讓餘額歸零。
//     注意：補償的是「歸零」，而非「恢復原值」。業務層應同步撤銷對應的虛擬道具。
//  3. 觸發外部警報（Slack / PagerDuty），通知業務團隊人工處置超扣的資源。
//
// ═══════════════════════════════════════════════════════════════
// 生產環境注意事項
// ═══════════════════════════════════════════════════════════════
//
//   - 全表掃描 wallet_events 在資料量大時成本極高。
//     建議維護一個獨立的 `wallet_uid_registry` 表，記錄所有已知 (tenant_id, uid)，
//     讓本任務可以逐一精準查詢而非全表掃描。
//
//   - Cassandra 本身不支援 SUM 聚合。需要在 Go 裡面逐行累加。
//     對於活躍玩家，快照機制（wallet_snapshots）會確保只需讀取少量 delta，
//     降低本任務的 I/O 開銷。
//
//   - 建議透過 K8s CronJob 或外部排程器呼叫本函式，而非在 Node 進程內定時執行，
//     以避免影響熱路徑效能。
func RunDailyReconciliation(ctx context.Context, store persistence.Store) error {
	log.Println("[reconciliation] starting daily reconciliation scan...")
	start := time.Now()

	// ── Step 1: 取得所有需要對帳的 (tenant_id, uid) 清單 ─────────────────────
	//
	// TODO: 實際生產環境應從獨立的 registry 表讀取，而非全表掃描。
	// 此處僅示意；全表掃描 wallet_events 的 DISTINCT 在大資料量下不可行。
	//
	//   SELECT DISTINCT tenant_id, uid FROM wallet_events;
	//
	// 暫以空 slice 作為佔位，迴圈不執行。

	type uidKey struct {
		tenantID int32
		uid      int64
	}

	// TODO: 替換為從 wallet_uid_registry 查詢的真實清單。
	var targets []uidKey

	successCount := 0
	discrepancyCount := 0

	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("reconciliation cancelled: %w", err)
		}

		balance, err := computeTrueBalance(ctx, store, target.tenantID, target.uid)
		if err != nil {
			log.Printf("[reconciliation] WARN: failed to compute balance for tenant=%d uid=%d: %v",
				target.tenantID, target.uid, err)
			continue
		}

		if balance < 0 {
			discrepancyCount++
			log.Printf("[reconciliation] ALERT: negative balance detected tenant=%d uid=%d balance=%d",
				target.tenantID, target.uid, balance)

			if err := issueCompensatingTransaction(ctx, store, target.tenantID, target.uid, balance); err != nil {
				log.Printf("[reconciliation] ERROR: failed to issue compensating tx for tenant=%d uid=%d: %v",
					target.tenantID, target.uid, err)
				continue
			}

			log.Printf("[reconciliation] compensating tx issued for tenant=%d uid=%d deficit=%d",
				target.tenantID, target.uid, balance)
		}

		successCount++
	}

	log.Printf("[reconciliation] completed in %v: checked=%d discrepancies=%d",
		time.Since(start), successCount, discrepancyCount)
	return nil
}

// computeTrueBalance 從 Cassandra 從頭計算指定 (tenant_id, uid) 的真實餘額。
//
// 流程（與 Actor.rehydrate 邏輯一致，但以審計為目的全量重算）：
//  1. 讀取最新快照 (balance, last_version)。
//  2. 讀取 version > last_version 的所有 delta events，按 version ASC 累加。
//  3. 回傳：snapshot_balance + sum(delta_amounts)。
//
// 注意：此函式不更新任何記憶體狀態，純讀取計算，可安全並行呼叫。
func computeTrueBalance(ctx context.Context, store persistence.Store, tenantID int32, uid int64) (int64, error) {
	// Step 1: 讀快照
	var snapshotBalance int64
	var lastVersion int64

	snapCQL := `SELECT balance, last_version FROM wallet_snapshots WHERE tenant_id = ? AND uid = ?`
	err := store.Query(ctx, snapCQL, []any{tenantID, uid}, func(s persistence.Scanner) error {
		for s.Scan(&snapshotBalance, &lastVersion) {
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("computeTrueBalance: load snapshot failed: %w", err)
	}

	// Step 2: 累加快照後的 Delta（以 version 定址，精準不漏）
	balance := snapshotBalance
	eventCQL := `SELECT delta_amount FROM wallet_events WHERE tenant_id = ? AND uid = ? AND version > ? ORDER BY version ASC`
	err = store.Query(ctx, eventCQL, []any{tenantID, uid, lastVersion}, func(s persistence.Scanner) error {
		var delta int64
		for s.Scan(&delta) {
			balance += delta
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("computeTrueBalance: load events failed: %w", err)
	}

	return balance, nil
}

// issueCompensatingTransaction 向 wallet_events 寫入一筆正向補償交易。
//
// 補償原則：
//   - 若真實餘額 = -500，寫入 delta_amount = +500，讓餘額歸零。
//   - 不恢復為扣款前的原始值，避免「多補」。
//   - TxID 格式：reconciliation-{tenantID}-{uid}-{unix_nano}，確保全域唯一。
//   - 此操作本身也是盲寫，若失敗需由排程器重試。
//
// ⚠️  Version 說明（TOCTOU 風險）：
//
//	補償交易需要決定正確的 version。此處先查詢當前最大 version，再 +1 寫入。
//	若此期間 live Actor 也在處理訊息，可能產生 version 衝突（兩筆都寫 maxVersion+1）。
//	在 Cassandra 中後者覆蓋前者（Last Write Wins），導致一筆事件被靜默覆蓋。
//	生產建議：對帳補償應透過 Actor 正常訊息路徑處理，以保證 version 單調性。
func issueCompensatingTransaction(ctx context.Context, store persistence.Store, tenantID int32, uid int64, negativeBalance int64) error {
	if negativeBalance >= 0 {
		return nil
	}

	// 查詢當前最大 version，補償交易寫入 maxVersion+1
	var maxVersion int64
	maxVersionCQL := `SELECT version FROM wallet_events WHERE tenant_id = ? AND uid = ? ORDER BY version DESC LIMIT 1`
	if err := store.Query(ctx, maxVersionCQL, []any{tenantID, uid}, func(s persistence.Scanner) error {
		s.Scan(&maxVersion)
		return nil
	}); err != nil {
		return fmt.Errorf("issueCompensatingTransaction: failed to get max version: %w", err)
	}

	deficit := -negativeBalance // deficit > 0
	now := time.Now()
	nextVersion := maxVersion + 1
	txID := fmt.Sprintf("reconciliation-%d-%d-%d", tenantID, uid, now.UnixNano())

	stmt := persistence.Statement{
		CQL:        `INSERT INTO wallet_events (tenant_id, uid, version, created_at, tx_id, delta_amount, payload) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		Args:       []any{tenantID, uid, nextVersion, now, txID, deficit, []byte("reconciliation-compensating-tx")},
		Idempotent: false,
	}
	return store.ExecuteBatch(ctx, persistence.UnloggedBatch, []persistence.Statement{stmt})
}
