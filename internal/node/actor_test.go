package node

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/frankieli/actor_cluster/pkg/persistence"
	pb "github.com/frankieli/actor_cluster/pkg/remote/pb"
)

// ─── Mocks ───────────────────────────────────────────────────────────────────

type mockStore struct {
	snapshot *snapshotRow
	events   []eventRow
	appended []persistence.Statement
}

type snapshotRow struct {
	balance     int64
	lastVersion int64
}

type eventRow struct {
	version int64
	txID    string
	delta   int64
}

func (m *mockStore) ExecuteBatch(_ context.Context, _ persistence.BatchMode, stmts []persistence.Statement) error {
	m.appended = append(m.appended, stmts...)
	return nil
}

func (m *mockStore) Query(_ context.Context, cql string, _ []any, scan func(persistence.Scanner) error) error {
	if strings.Contains(cql, "wallet_snapshots") {
		return scan(&snapshotScanner{row: m.snapshot})
	}
	return scan(&eventScanner{rows: m.events})
}

func (m *mockStore) Close() {}

type snapshotScanner struct {
	row  *snapshotRow
	done bool
}

func (s *snapshotScanner) Scan(dest ...any) bool {
	if s.done || s.row == nil {
		return false
	}
	s.done = true
	*dest[0].(*int64) = s.row.balance
	*dest[1].(*int64) = s.row.lastVersion
	return true
}

type eventScanner struct {
	rows []eventRow
	idx  int
}

func (s *eventScanner) Scan(dest ...any) bool {
	if s.idx >= len(s.rows) {
		return false
	}
	*dest[0].(*int64) = s.rows[s.idx].version
	*dest[1].(*string) = s.rows[s.idx].txID
	*dest[2].(*int64) = s.rows[s.idx].delta
	s.idx++
	return true
}

type mockSink struct {
	results []*pb.RemoteResult
}

func (m *mockSink) Emit(result *pb.RemoteResult) {
	m.results = append(m.results, result)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func encodeAmount(amount int64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(amount))
	return buf
}

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestBusinessActor_Rehydrate_NoSnapshot(t *testing.T) {
	store := &mockStore{
		snapshot: nil,
		events: []eventRow{
			{version: 1, txID: "tx1", delta: 200},
			{version: 2, txID: "tx2", delta: -50},
		},
	}
	bActor := newBusinessActor(1, 1001, store, &mockSink{})
	if err := bActor.rehydrate(context.Background()); err != nil {
		t.Fatalf("rehydrate failed: %v", err)
	}
	if bActor.balance != 150 {
		t.Errorf("expected balance 150, got %d", bActor.balance)
	}
	if bActor.version != 2 {
		t.Errorf("expected version 2, got %d", bActor.version)
	}
	if _, ok := bActor.txCache["tx1"]; !ok {
		t.Error("tx1 should be in txCache")
	}
	if _, ok := bActor.txCache["tx2"]; !ok {
		t.Error("tx2 should be in txCache")
	}
}

func TestBusinessActor_Rehydrate_WithSnapshot(t *testing.T) {
	store := &mockStore{
		// 快照涵蓋 version 1~10，餘額 1000
		snapshot: &snapshotRow{balance: 1000, lastVersion: 10},
		events: []eventRow{
			{version: 11, txID: "tx-after-snap", delta: -300},
		},
	}
	bActor := newBusinessActor(2, 2002, store, &mockSink{})
	if err := bActor.rehydrate(context.Background()); err != nil {
		t.Fatalf("rehydrate failed: %v", err)
	}
	if bActor.balance != 700 {
		t.Errorf("expected balance 700, got %d", bActor.balance)
	}
	if bActor.version != 11 {
		t.Errorf("expected version 11, got %d", bActor.version)
	}
}

func TestBusinessActor_Rehydrate_VersionGap(t *testing.T) {
	store := &mockStore{
		snapshot: nil,
		events: []eventRow{
			{version: 1, txID: "tx1", delta: 100},
			{version: 3, txID: "tx3", delta: 50}, // version 2 is missing
		},
	}
	bActor := newBusinessActor(1, 1001, store, &mockSink{})
	err := bActor.rehydrate(context.Background())
	if err == nil {
		t.Fatal("expected error for version gap, got nil")
	}
	if !strings.Contains(err.Error(), "version gap") {
		t.Errorf("expected 'version gap' in error, got: %v", err)
	}
}

func TestBusinessActor_Handle(t *testing.T) {
	store := &mockStore{
		// 快照涵蓋 version 1~5，餘額 100
		snapshot: &snapshotRow{balance: 100, lastVersion: 5},
	}
	sink := &mockSink{}
	bActor := newBusinessActor(1, 1001, store, sink)
	if err := bActor.rehydrate(context.Background()); err != nil {
		t.Fatalf("rehydrate failed: %v", err)
	}
	if bActor.balance != 100 {
		t.Errorf("expected balance 100 after rehydrate, got %d", bActor.balance)
	}
	if bActor.version != 5 {
		t.Errorf("expected version 5 after rehydrate, got %d", bActor.version)
	}

	bActor.txCache["tx-already-written"] = struct{}{}

	tests := []struct {
		name          string
		env           *pb.RemoteEnvelope
		expectSuc     bool
		expectBal     int64
		expectVersion int64
	}{
		{
			name: "Idempotent: same TxID returns success without re-writing",
			env:  &pb.RemoteEnvelope{TxId: "tx-already-written", Payload: encodeAmount(-50)},
			// Payload 為空：txCache 無法重建原始結算 balance，不回傳以免誤導 gateway。
			expectSuc:     true,
			expectBal:     0, // empty payload expected
			expectVersion: 5, // version unchanged
		},
		{
			name:      "Insufficient funds",
			env:       &pb.RemoteEnvelope{TxId: "tx-insufficient", Payload: encodeAmount(-200)},
			expectSuc: false,
		},
		{
			name:          "Valid deduction",
			env:           &pb.RemoteEnvelope{TxId: "tx-deduct", Payload: encodeAmount(-50)},
			expectSuc:     true,
			expectBal:     50,
			expectVersion: 6,
		},
		{
			name:          "Valid addition",
			env:           &pb.RemoteEnvelope{TxId: "tx-add", Payload: encodeAmount(100)},
			expectSuc:     true,
			expectBal:     150,
			expectVersion: 7,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			res := bActor.Handle(tt.env)
			if res == nil {
				t.Fatal("Handle returned nil result")
			}
			if res.Success != tt.expectSuc {
				t.Errorf("expected success=%v, got %v (errCode=%s msg=%s)",
					tt.expectSuc, res.Success, res.ErrorCode, res.ErrorMsg)
			}
			if res.Success && tt.expectBal != 0 {
				actualBal := int64(binary.BigEndian.Uint64(res.Payload))
				if actualBal != tt.expectBal {
					t.Errorf("expected balance %d, got %d", tt.expectBal, actualBal)
				}
			}
			if tt.expectVersion != 0 && bActor.version != tt.expectVersion {
				t.Errorf("expected version %d, got %d", tt.expectVersion, bActor.version)
			}
		})
	}
}

func TestBusinessActor_Handle_VersionMonotonicallyIncreases(t *testing.T) {
	store := &mockStore{snapshot: &snapshotRow{balance: 0, lastVersion: 0}}
	bActor := newBusinessActor(1, 1001, store, &mockSink{})
	if err := bActor.rehydrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 5; i++ {
		bActor.Handle(&pb.RemoteEnvelope{
			TxId:    fmt.Sprintf("tx-%d", i),
			Payload: encodeAmount(10),
		})
		if bActor.version != int64(i) {
			t.Errorf("after Handle #%d: expected version=%d, got %d", i, i, bActor.version)
		}
	}
}

func TestBusinessActor_SnapshotThreshold(t *testing.T) {
	store := &mockStore{
		snapshot: &snapshotRow{balance: 0, lastVersion: 0},
	}
	bActor := newBusinessActor(1, 999, store, &mockSink{})
	if err := bActor.rehydrate(context.Background()); err != nil {
		t.Fatalf("rehydrate failed: %v", err)
	}

	for i := 0; i < snapshotThreshold-1; i++ {
		res := bActor.Handle(&pb.RemoteEnvelope{
			TxId:    fmt.Sprintf("tx-snap-%d", i),
			Payload: encodeAmount(1),
		})
		if !res.Success {
			t.Fatalf("Handle[%d] should succeed, got err: %s", i, res.ErrorMsg)
		}
	}
	if bActor.eventCount != snapshotThreshold-1 {
		t.Errorf("expected eventCount=%d, got %d", snapshotThreshold-1, bActor.eventCount)
	}

	res := bActor.Handle(&pb.RemoteEnvelope{TxId: "tx-trigger-snapshot", Payload: encodeAmount(1)})
	if !res.Success {
		t.Fatalf("final Handle should succeed: %s", res.ErrorMsg)
	}
	if bActor.eventCount != 0 {
		t.Errorf("after snapshot trigger, expected eventCount=0, got %d", bActor.eventCount)
	}
	// version 應等於 snapshotThreshold（共處理了 snapshotThreshold 筆成功事件）
	if bActor.version != int64(snapshotThreshold) {
		t.Errorf("expected version=%d, got %d", snapshotThreshold, bActor.version)
	}
}
