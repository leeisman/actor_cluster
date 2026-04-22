package discovery

import (
	"errors"
	"testing"
)

func TestModuloSlotOwner(t *testing.T) {
	t.Parallel()

	nodes := []string{"10.0.0.1:9000", "10.0.0.2:9000", "10.0.0.3:9000"}

	tests := []struct {
		slot   int
		wantIP string
	}{
		{slot: 0, wantIP: "10.0.0.1:9000"},
		{slot: 1, wantIP: "10.0.0.2:9000"},
		{slot: 2, wantIP: "10.0.0.3:9000"},
		{slot: 3, wantIP: "10.0.0.1:9000"},
		{slot: 4, wantIP: "10.0.0.2:9000"},
	}

	for _, tc := range tests {
		if got := ModuloSlotOwner(nodes, tc.slot); got != tc.wantIP {
			t.Fatalf("ModuloSlotOwner(slot=%d) = %q, want %q", tc.slot, got, tc.wantIP)
		}
	}
	if g := ModuloSlotOwner(nil, 0); g != "" {
		t.Fatalf("ModuloSlotOwner(nil,0) = %q, want empty", g)
	}
}

func TestBuildRoutingTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		nodes     []string
		checkSlot int
		wantIP    string
	}{
		{
			name:      "空節點列表會回傳全空表",
			nodes:     nil,
			checkSlot: 0,
			wantIP:    "",
		},
		{
			name:      "單節點：所有 slot 都指向同一個 IP",
			nodes:     []string{"10.0.0.1:9000"},
			checkSlot: 777,
			wantIP:    "10.0.0.1:9000",
		},
		{
			name:      "雙節點：使用 modulo 均分 slots",
			nodes:     []string{"10.0.0.1:9000", "10.0.0.2:9000"},
			checkSlot: 513,
			wantIP:    "10.0.0.2:9000",
		},
		{
			name:      "三節點：slot 1023 仍使用 modulo",
			nodes:     []string{"10.0.0.1:9000", "10.0.0.2:9000", "10.0.0.3:9000"},
			checkSlot: 1023,
			wantIP:    "10.0.0.1:9000",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			table := BuildRoutingTable(tc.nodes, nil)
			if got := table[tc.checkSlot]; got != tc.wantIP {
				t.Fatalf("table[%d] = %q, want %q", tc.checkSlot, got, tc.wantIP)
			}
		})
	}
}

func TestSlotOf_Range(t *testing.T) {
	t.Parallel()

	tests := []struct {
		tenantID int32
		uid      int64
	}{
		{1, 0},
		{1, 1},
		{1, 1023},
		{1, 1024},
		{2, 1024},
		{99, 9999999},
	}

	for _, tc := range tests {
		got := SlotOf(tc.tenantID, tc.uid)
		if got < 0 || got >= PartitionSlots {
			t.Fatalf("SlotOf(%d, %d) = %d, want within [0, %d)", tc.tenantID, tc.uid, got, PartitionSlots)
		}
	}
}

func TestSlotOf_UsesTenantID(t *testing.T) {
	t.Parallel()

	const uid int64 = 777777
	slotA := SlotOf(1, uid)
	slotB := SlotOf(2, uid)
	if slotA == slotB {
		t.Fatalf("SlotOf should include tenantID in route key, got same slot=%d for uid=%d", slotA, uid)
	}
}

func TestGetNodeIP_Table(t *testing.T) {
	t.Parallel()

	nodes := []string{"10.0.0.1:9000", "10.0.0.2:9000"}
	table := BuildRoutingTable(nodes, nil)
	tenantID := int32(1)
	uid := int64(100)
	want := nodes[SlotOf(tenantID, uid)%len(nodes)]
	got, err := GetNodeIP(table, tenantID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("GetNodeIP = %q, want %q", got, want)
	}

	_, err = GetNodeIP(nil, 1, 1)
	if !errors.Is(err, ErrTableNotInitialized) {
		t.Fatalf("nil table: %v", err)
	}

	empty := BuildRoutingTable(nil, nil)
	_, err = GetNodeIP(empty, 1, 1)
	if !errors.Is(err, ErrNoOwner) {
		t.Fatalf("empty table: %v", err)
	}
}
