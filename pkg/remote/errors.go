// Package remote 的穩定錯誤字串：用於 gRPC status 的 message，或
// pb.RemoteResult.error_code（兩邊**共用同一組字面值**，利於觀測與測試）。
// 客戶端僅在收到 BatchResponse 前產生之「合成」結果亦使用下述 ERR_* 常值。
// 完整語意與歸屬見 docs/design/04_remote_spec.md §7。
package remote

const (
	// --- gRPC transport（pkg/remote.Server 內以 status 回傳，message 為下列字串）---
	ErrBadRequest       = "ERR_BAD_REQUEST"       // nil batch/response 或不可解析之請求
	ErrTransportClosed  = "ERR_TRANSPORT_CLOSED"  // stream Recv/Send 失敗、EOF、或 context 取消
	ErrBatchTooLarge    = "ERR_BATCH_TOO_LARGE"   // len(envelopes) 超過 ServerConfig.MaxBatchSize
	ErrDeadlineExceeded = "ERR_DEADLINE_EXCEEDED" // handler 回傳 context.DeadlineExceeded
	ErrInternal         = "ERR_INTERNAL"          // handler 回傳非 gRPC status 的未知錯誤

	// --- RemoteResult.error_code：Node / handler（從 gRPC 送達客戶端）---
	ErrWrongNode         = "ERR_WRONG_NODE"         // 本節點非 (tenant,uid) 之 owner
	ErrActorUnavailable  = "ERR_ACTOR_UNAVAILABLE"  // Actor 無法接受訊息（如尚未就緒；預留）
	ErrPayloadTooLarge   = "ERR_PAYLOAD_TOO_LARGE"  // 單筆 payload 超過上層硬上限（與傳輸層分開）
	ErrRehydrationFailed = "ERR_REHYDRATION_FAILED" // 自 Cassandra 還原 Actor 狀態失敗
	ErrNodeOverloaded    = "ERR_NODE_OVERLOADED"    // node 級 mailbox backlog 已達上限，拒絕再 enqueue

	// 業務（wallet 節點示例）：在 RemoteResult 中與上列同一欄位
	ErrInvalidPayload    = "ERR_INVALID_PAYLOAD"    // 例如 payload 長度或格式不合法
	ErrInsufficientFunds = "ERR_INSUFFICIENT_FUNDS" // 業務防護，餘額不足
	ErrPersistenceFailed = "ERR_PERSISTENCE_FAILED" // 底層 ExecuteBatch/Query 失敗

	// Client 在尚未取得伺服器回應時合成之結果（不經 gRPC；仍寫入 error_code 供基準測試與重試邏輯使用）
	ErrDiscovery         = "ERR_DISCOVERY"          // GetNodeIP 失敗（表未就緒、無 owner 等）
	ErrConnection        = "ERR_CONNECTION"         // 無法建連/取得 NodeStreamer
	ErrStreamInterrupted = "ERR_STREAM_INTERRUPTED" // 串流 Recv 錯誤導致未完成之請求一併失敗
)
