// Package mock 提供各模組 Interface 的測試替身 (Test Doubles)。
// 這些 Mock 物件由 internal/mock 持有，確保不污染 pkg/ 的核心框架程式碼。
//
// 使用方式：在 *_test.go 中引入此包，透過依賴注入將 MockResolver
// 傳入任何接受 discovery.TopologyResolver 介面的元件。
package mock

import (
	"context"
	"fmt"
	"sync"
)

// MockResolver 實作 discovery.TopologyResolver 介面。
//
// 設計目標：
//  1. 允許測試精準控制「哪個 (TenantID, UID) 應該路由到哪個 IP」。
//  2. 允許測試模擬 GetNodeIP 失敗（透過 SetError）。
//  3. 記錄所有 GetNodeIP 呼叫次數（AssertCallCount）以驗行為。
//
// 注意：MockResolver 在測試中通常是單 goroutine 使用，
// 但萬一多 goroutine 並發，也用 mu 保護，確保 race detector 不報警。
type MockResolver struct {
	mu        sync.Mutex
	routes    map[RouteKey]string // (tenantID, uid) → host:port
	forcedErr error               // 若不為 nil，GetNodeIP 一律回傳此 error
	callCount int                 // 記錄 GetNodeIP 總呼叫次數
}

// RouteKey 是 MockResolver 專用的 deterministic routing key。
type RouteKey struct {
	TenantID int32
	UID      int64
}

// NewMockResolver 建立一個空的 MockResolver。
// 透過 SetRoute/SetError 設定測試情境。
func NewMockResolver() *MockResolver {
	return &MockResolver{
		routes: make(map[RouteKey]string),
	}
}

// SetRoute 設定指定 (tenantID, uid) 應路由至的 ip（格式為 "host:port"）。
func (m *MockResolver) SetRoute(tenantID int32, uid int64, ip string) {
	m.mu.Lock()
	m.routes[RouteKey{TenantID: tenantID, UID: uid}] = ip
	m.mu.Unlock()
}

// SetError 設定 GetNodeIP 一律回傳的強制錯誤。
// 傳入 nil 可清除強制錯誤，恢復正常路由行為。
func (m *MockResolver) SetError(err error) {
	m.mu.Lock()
	m.forcedErr = err
	m.mu.Unlock()
}

// CallCount 回傳 GetNodeIP 截至目前為止的總呼叫次數。
func (m *MockResolver) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

// --- 實作 discovery.TopologyResolver 介面 ---

// GetNodeIP 查詢 (tenantID, uid) 對應的路由 IP。
// 若有強制錯誤，直接回傳；否則查表，找不到回傳「未指派」錯誤。
func (m *MockResolver) GetNodeIP(tenantID int32, uid int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.callCount++

	if m.forcedErr != nil {
		return "", m.forcedErr
	}

	key := RouteKey{TenantID: tenantID, UID: uid}
	ip, ok := m.routes[key]
	if !ok {
		return "", fmt.Errorf("mock: no route for tenant_id=%d uid=%d", tenantID, uid)
	}

	return ip, nil
}

// Watch 為 Mock 的空實作，不啟動任何背景 goroutine。
// 在測試中，路由表由 SetRoute 手動控制，無需 Watch。
func (m *MockResolver) Watch(_ context.Context) error {
	return nil
}

// Close 為 Mock 的空實作，無任何真實資源需要釋放。
func (m *MockResolver) Close() error {
	return nil
}
