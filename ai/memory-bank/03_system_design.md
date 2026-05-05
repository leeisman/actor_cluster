# System Design (架構概覽與系統邊界)

本文件定義系統的高階拓樸、元件職責與核心設計模式。具體實作細節（如 Schema、具體運算邏輯、錯誤代碼處理）應交由 `docs/design/` 下的詳細規格書去定義。

## 目錄與元件劃分 (Directory Structure)

專案結構遵循高內聚、低耦合原則，以下是一目瞭然的實體目錄規劃與職責說明：

```text
├── cmd/                       # 程式的執行入口 (Entrypoints)
│   ├── node/                  # Actor Server 實體：啟動 Actor 引擎、gRPC 監聽與監控服務
│   └── client/                # 發射端實體：掛載 TopologyResolver 並執行 Batch gRPC 發送
├── pkg/                       # 「無外部 Actor 框架」的核心原始碼 (僅引入 gRPC/etcd/DB 等必備驅動)
│   ├── actor/                 # 極薄 Actor runtime：MPSC Mailbox、單 goroutine callback、ResultSink emit
│   ├── discovery/             # 服務發現與槽位運算層：etcd 監聽實作與 Slot 映射介面
│   ├── persistence/           # DB 儲存層：Cassandra Event 讀寫 primitive 與 Snapshot 行讀寫
│   ├── remote/                # 網路通訊層：gRPC protobuf 與 Node 端 transport adapter
│   └── utils/                 # 極限效能工具包：`sync.Pool` 封裝、無鎖(Lock-Free)結構等
├── internal/                  # 專案私有代碼，存放 Node runtime 與測試輔助
│   ├── node/                  # Actor Node 應用層：接 BatchRequest、找 Actor、dispatch、聚合 response
│   └── mock/                  # 業務級別的測試 Mock 代碼
├── docs/                      # 系統規格書與維護文件
│   └── design/                # 由 `/spec` 生成的深度架構與演算法設計檔案
└── ai/                        # AI 開發大腦，與專案密不可分，納入版控一同演進
```

## 網路拓樸與路由 (Networking & Routing)
1. **Client 端直連架構**: Client (Gateway) 負責維護尋址機制。透過 `TopologyResolver` 介面監聽 etcd 中的存活 Node IP（`/actor_cluster/nodes/{ip}` ephemeral key），在本地以字典序排序 + 統一演算法建立 `[1024]string` routing table，查詢 `SlotOf(TenantID, UID)` 後直連目標 Node，並將相同目標 IP 的請求打包成 Batch 發送。
2. **無中間節點轉發 (No Multi-hop)**: Cluster 內部 Node 與 Node 之間不互相轉發封包，維持架構扁平以縮減延遲。
3. **Remote 僅作為 Transport Adapter**: `pkg/remote` 只負責 protobuf contract、`ActorService.StreamMessages` 的 gRPC 收發、`BatchHandler` callback 與 `BatchSender.SendBatchResponse` 的 stream send 序列化。它不持有 Actor dispatcher、不做 ownership / op_code / payload / deadline 等語義驗證、不決定 worker pool 或 response batching policy。
4. **Node Runtime 承擔 Actor 調度策略**: `cmd/node` 應只負責組裝，實際 Node runtime handler 負責接收 `BatchRequest` (可能夾帶上百個不同 UID 的 Envelope)。為了應付每毫秒一發的極高併發 Batch 拆解，Node 內部使用 256 個 Shard 的 Map 來降低尋找 Actor 時的鎖競爭。它負責 envelope validation、Actor 路由、同 `(TenantID, UID)` 保序、不同 Actor 並行，以及按時間或數量門檻合併 `BatchResponse`。

## 持久化與狀態重現 (Persistence & Rehydration)
系統採用**純粹 Event Sourcing（盲寫 Append-Only）+ Actor 記憶體單執行緒防護**，拋棄 LWT/CAS 等強一致性鎖，選擇 AP（高可用）策略：

1. **Event Sourcing 與 Version 序號**：`wallet_events` 存入 `(version, delta_amount, payload)` 等。Version 由 Actor goroutine 在單執行緒記憶體內嚴格單調遞增指派，取代精度不足的 `created_at` 作為精準的 Ordering/Clustering Key。
2. **兩段式 Rehydrate**：
   - Step 1：讀 `wallet_snapshots` 取得最近快照的 `(balance, last_version)`。
   - Step 2：讀 `wallet_events WHERE version > last_version`，累加 Delta 重建餘額與狀態，並填充 TxID Cache。若發現 version 跳號立即報錯。
3. **快照（Snapshot）**：每 1000 筆成功事件非同步快照（fire-and-forget）。**刻意不在關機時強制存快照**：10 萬個 Actor 同時 UPSERT 會造成 Cassandra 雪崩並拖長關機時間（Shutdown Thundering Herd）。未快照的 Delta Events 已安全落地，rehydrate 成本由接手節點分散承擔。
4. **記憶體守門**：餘額檢查在 Actor 單 goroutine 內完成（`balance + amount >= 0`），是唯一的業務鎖，無需 DB 層的強一致性。
5. **超扣補償（AP 代價）**：極端腦裂下可能發生負餘額，由每日 `RunDailyReconciliation` 從 Cassandra 全量重算，對差值發補償交易（Compensating Transaction）並觸發業務警報。

## I/O 邊界批次化 (I/O Boundary Batching)
在「系統邊界」進行對外網路或硬碟操作時，必須實作 Batching，以降低吞吐瓶頸：
1. **網路發送邊界**: Client 對 Node 送信前，透過時間與數量門檻進行打包。
2. **網路回應邊界**: Node runtime 收集 Actor response 後，可依單位時間或單位數量合併成 `BatchResponse`，再呼叫 `remote.BatchSender.SendBatchResponse`；`pkg/remote` 只序列化 stream Send，不實作合併策略。
3. **硬碟持久化邊界**: Node 發往 Cassandra 落檔前，一併進行打包。(此外部環節准許有限度使用 `chan` 作為緩衝，與純無鎖的 `pkg/actor` runtime 隔開)

## 依賴注入與核心介面 (Dependency Injection & Interfaces)
為確保系統具備極高的單元測試覆蓋率與可測試性 (Testability)，並能透過 Mock 工具隔離沉重的外部 I/O，系統在外部邊界必須嚴格採用依賴注入 (DI)。遵循 Go 語言的「Accept Interfaces, Return Structs」心法：
1. **`TopologyResolver` (尋址層介面)**：必須介面化。隔離 `etcd` 或底層 K8s 的實作，Client 僅透過此介面傳入 `(TenantID, UID)` 來取得 `Node IP`，方便測試時抽換成 `MockResolver`。
2. **`persistence.Store` (儲存 primitive 介面)**：`pkg/persistence` 只隔離 Cassandra session、batch statement execution、query scanner callback。`WalletEvent`、`wallet_events` CQL、partition/version guard、rehydration 與 TxID cache 都屬於 `internal/node` 的 business store / Actor handler，不放在 `pkg/persistence`。
3. **`BatchHandler` / `BatchSender` (Remote Transport 邊界介面)**：`pkg/remote` 透過 `BatchHandler` 將收到的 `BatchRequest` 回呼給 Node runtime，並透過 `BatchSender` 提供同一 stream 的 `BatchResponse` 送出口。這是 transport adapter 與 Node runtime 的唯一銜接點。
4. **`actor.Handler` / `actor.ResultSink` (Actor Runtime 邊界介面)**：`pkg/actor` 只負責把 `*pb.RemoteEnvelope` 依序交給 callback，並把 callback result emit 到 Actor 建立時注入的 sink。業務邏輯、持久化、冪等、result routing 與 response batching 都由 `internal/node` 的 handler / sink 實作承擔。
   - **CQRS Payload 語義**：`Handle()` 屬於 Command Path，`*pb.RemoteResult.Payload` 僅在**正常成功寫入**時填入當次結算後的餘額；冪等重打回傳空 Payload（`txCache` 無法重建原始結算餘額，回傳當前餘額會產生語義歧義）。需要餘額必須走獨立的 Read Path。

## 分佈式系統陷阱與架構容錯 (Distributed Systems Resilience)

1. **Cassandra Batch 必須屬於同一 Partition**
   - Actor 每次只寫自己的 `(TenantID, UID)`，天然滿足單 Partition 限制，不需要額外防呆。

2. **幽靈寫入（Phantom Write）的防護**
   - 盲寫 + 記憶體守門：Actor 只在記憶體判斷餘額，寫入 DB 後若 ACK 丟失，Client 用相同 TxID 重試，txCache 防重放直接視為成功，不會重複扣款。
   - 補充說明：PRIMARY KEY 包含 `version`。同一 TxID 的重試依賴 Actor 在記憶體由 `txCache` 攔截去重。txCache 是唯一的防重放防線，覆蓋範圍至少涵蓋快照後的 1000 筆。

3. **腦裂（Split-Brain）防護**
   - 主動防護：`forceKillLocalActor` + `WRONG_NODE` 錯誤，讓 Gateway 即時感知並 Retry 到正確節點。
   - 被動補償：`RunDailyReconciliation` 每日從 DB 重算真實餘額，對超扣發補償交易。

4. **Cloud-Native 自我感知**：
   - `resolveMyIP` 三段優先順序：`--ip` flag → `POD_IP` env（K8s Downward API）→ UDP 探測網卡，支援本地開發 / VM / K8s 一致啟動流程。
