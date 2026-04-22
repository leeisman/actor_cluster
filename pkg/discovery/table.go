// 槽位雜湊、預建 [PartitionSlots]string 與純表 GetNodeIP。EtcdResolver 以 atomic.Pointer[Table] 持表。
package discovery

import "errors"

// PartitionSlots 定義系統寫死的最大切分槽位數量。
// 1024 = 2^10，讓 mix(tenantID, uid) 可用位元 AND 替代模數運算（見 SlotOf）。
const PartitionSlots = 1024

const slotMask = PartitionSlots - 1

// Table 是固定長度的節點位址陣列，索引即 Slot ID。預建表讓讀路徑 O(1) 且零 alloc。
type Table [PartitionSlots]string

// SlotOf 將 (tenantID, uid) 穩定映射到 [0, PartitionSlots) 的槽位。
// 使用固定 SplitMix64 finalizer、無隨機種子，保證各進程結果一致、且不產生 heap alloc。
func SlotOf(tenantID int32, uid int64) int {
	x := uint64(uint32(tenantID))<<32 ^ uint64(uid)
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return int(x & slotMask)
}

// SlotOwnerFunc 根據「已排序」的節點位址與 slot，回傳負責該 slot 的 host:port。
type SlotOwnerFunc func(sortedNodeAddrs []string, slot int) string

// ModuloSlotOwner 是預設的 slot 分配：sortedNodeAddrs[slot % n]。
// sortedNodeAddrs 必須已字典序排序；若切片為空則回傳空字串（不觸發除零）。
func ModuloSlotOwner(sortedNodeAddrs []string, slot int) string {
	if len(sortedNodeAddrs) == 0 {
		return ""
	}
	return sortedNodeAddrs[slot%len(sortedNodeAddrs)]
}

// BuildRoutingTable 依已排序的節點位址建預展開路由表。owner 可為 nil，此時等於使用 ModuloSlotOwner。
// 當節點清單為空時，回傳的表內全為空字串。
func BuildRoutingTable(sortedNodeAddrs []string, owner SlotOwnerFunc) *Table {
	var t Table
	if len(sortedNodeAddrs) == 0 {
		return &t
	}
	ownerFn := owner
	if ownerFn == nil {
		ownerFn = ModuloSlotOwner
	}
	for slot := 0; slot < PartitionSlots; slot++ {
		t[slot] = ownerFn(sortedNodeAddrs, slot)
	}
	return &t
}

// 與 ErrRoutingTableNotInitialized / ErrSlotUnassigned 別名同值，方便 errors.Is。
var (
	// ErrTableNotInitialized 表示未載入任何路由表（例如 *Table 為 nil）。
	ErrTableNotInitialized = errors.New("discovery: routing table not initialized")
	// ErrNoOwner 表示該 slot 在表中無對應節點（空字串）。
	ErrNoOwner = errors.New("discovery: slot is not assigned to any node")
)

// GetNodeIP 從單次快照表 *Table 解出 (tenantID, uid) 的負責節點 host:port（SlotOf → 讀表）。
// 叢集內由 EtcdResolver 等以 atomic.Pointer[Table] 釋出快照再呼叫本函式。
//
// 熱路徑：無鎖、無 defer、成功路徑無額外 heap 分配（若 table 非 nil）。
func GetNodeIP(table *Table, tenantID int32, uid int64) (string, error) {
	if table == nil {
		return "", ErrTableNotInitialized
	}
	ip := table[SlotOf(tenantID, uid)]
	if ip == "" {
		return "", ErrNoOwner
	}
	return ip, nil
}
