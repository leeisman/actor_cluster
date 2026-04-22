// Package discovery 提供叢集拓樸（etcd 等）與 (TenantID,UID)→節點 host:port 的單一實作。
// 熱路徑：GetNodeIP 高頻、零鎖、零 defer。槽位/建表/atomic 換表同在本套件。
package discovery

import "context"

// 向後兼容命名；與 errors.Is 搭配時與 ErrTableNotInitialized / ErrNoOwner 同值。
var (
	ErrRoutingTableNotInitialized = ErrTableNotInitialized
	ErrSlotUnassigned             = ErrNoOwner
)

// RoutingTable 即本套件之 Table（歷史命名）。
type RoutingTable = Table

// TopologyResolver 定義了將 Actor route key 映對至實體網路位置的合約。
// 實作者必須保證 GetNodeIP 在熱點路徑上滿足：
//   - 無任何 Mutex 等待
//   - 無任何 defer 呼叫
//   - 無任何 Heap 記憶體分配
//
// 目前提供的實作：
//   - EtcdResolver：從 etcd 拉取拓樸並持續 Watch 更新
//   - 未來可擴充：K8sResolver（替換 etcd，讀取 K8s Endpoints）
type TopologyResolver interface {
	// GetNodeIP 接收 Actor route key (tenantID, uid)，
	// 透過 SlotOf(tenantID, uid) 計算 SlotID，並回傳應直連的目標「host:port」字串。
	// 此方法必須在納秒 (ns) 級別完成，呼叫方不可假設有任何 I/O 容忍度。
	GetNodeIP(tenantID int32, uid int64) (string, error)

	// Watch 啟動背景拓樸同步 goroutine，持續監聽外部註冊中心的變更事件。
	// 當節點上下線時，Watch 負責以 Copy-on-Write 方式原子更新路由表。
	// ctx 用於控制 Watch goroutine 的生命週期，呼叫方應傳入可取消的 context。
	Watch(ctx context.Context) error

	// Close 優雅地關閉與外部註冊中心的連線，並釋放所有相關資源。
	Close() error
}
