# Technical Decisions (技術決策)

本文件紀錄了本專案底層架構的核心技術決策及其背後的工程考量。

## 1. 揚棄原生 Go Channel，改採 MPSC Queue

**決策**：在 Actor 的郵箱 (Mailbox) 與高頻核心通訊路徑上，棄用原生的 `chan`，全面採用 MPSC (Multi-Producer Single-Consumer) 自旋/無鎖隊列。

**背景與理由**：
- **鎖競爭與效能瓶頸**：Go 的原生 channel 內部依賴 `mutex` 來保護環形緩衝區與等待佇列。在多生產者場景下，channel 會遭遇嚴重的鎖競爭。
- **Goroutine 調度開銷**：channel 的阻塞會導致 Goroutine 頻繁觸發 `gopark` 和 `goready`，增加 Scheduler 的負擔與 Context Switch 成本。
- **MPSC 優勢**：Actor 模型中每個 Actor 只有一個執行緒（Consumer），但可能有多個來源請求（Producers）。無鎖的 MPSC Queue（基於 Ring Buffer 和 CAS 操作）能完美匹配，大幅減少 CPU Cache 顛簸與系統呼叫開銷。

## 2. 外部 Client 透過 gRPC 直連 Actor Node (節點間互不對話)

**決策**：Client 端透過 gRPC 直接與負責該 Actor 的目標節點連線。Cluster 內部的 Actor Node 彼此之間**不溝通、不互相轉發請求**。
且因為 `(TenantID, UID)` 是 Actor 的路由與資料所有權鍵，**所有與 Actor 相關的 gRPC 請求定義中，必定要同時帶有 `TenantID` 與 `UID` 欄位**。

**背景與理由**：
- 取消 Node-to-Node 通訊能大幅簡化網路拓樸與除錯難度，降低內部不可控的系統複雜度。
- Client 端自行進行路由計算，發送請求時就已精準命中目標。

## 3. 靜態槽位分配 (Static Slot Allocation) 與 etcd 註冊中心

**決策**：捨棄動態算出玩家在何處的 Dynamic Sharding，改採基於 Hash 的固定槽位分配 (Slot-based Routing)。使用 `(TenantID, UID)` 作為 Route Key，透過穩定 hash 算出 `hash(TenantID, UID) % 1024` 的固定槽位，並透過讀取「1024 槽位對應到哪一個 IP」的拓樸表來決定連線對象。這份拓樸表初期將存放在 **etcd** 中。

**背景與理由**：
- **避免腦裂 (Split-Brain) 與資料損壞**：動態路由在網路延遲或斷線造成的 Split-Brain 期間，極易導致兩個 Node 同時啟動並操作同一個 Actor，造成資料競爭。初期先以較保險的靜態分佈策略推進，雖然可能存在特定節點下線時的單點故障 (SPOF) 問題，但先求穩定安全，後續再針對 High Availability 做進階優化。
- **K8s 無痛轉移介面**：我們確保透過介面化 (Interface) 來抽離「獲取 1024 分佈表」的實作。目前實作為 etcd 版本，未來上 Kubernetes 時只要抽換該介面，改由 K8s API (或是 ConfigMap/CRD) 獲取 IP 分佈，即可無痛轉移。

### 3.1 禁止使用動態/隨機 Port 啟動（Static Port Policy）

**決策**：Node 啟動時必須透過 `--port` 參數或環境變數明確定義監聽 Port（預設 `:50051`），**嚴禁使用 `:0` 讓作業系統隨機分配**。

**核心原因（Actor 狀態一致性）**：

本系統為 **Stateful Actor Cluster**。節點的 `IP:Port` 是寫入 etcd 的唯一識別標記（Unique Identity）。
`pkg/discovery` 依賴這些識別標記進行字典序排序，分配 1024 個 Slot。

若使用隨機 Port，節點重啟後 Port 改變會被視為「新節點加入」與「舊節點離開」，觸發大量的 Slot 重分配（Re-sharding），造成：

- **幽靈節點**：etcd 中殘留大量舊 Port 的失效 key，干擾 Discovery 計算
- **Cassandra 寫入風暴**：大量 Actor 需重新 Rehydrate，`wallet_events` 的 SELECT 查詢瞬間激增
- **記憶體抖動**：Actor 狀態轉換、txCache 重建造成不必要的 GC 壓力
- **除錯困難**：隨機 Port 讓 Log 追蹤與 gRPC Stream 歸因變得不可能

**開發守則**：
- 本地多節點測試：手動指定不同 Port（如 `-port=:50051`、`-port=:50052`）
- K8s 環境：依賴 Pod 獨立 IP + 固定 Container Port，確保 `IP:Port` 唯一性

## 4. Command/Query 分離（CQRS）：Handle 不回傳狀態快照

**決策**：`BusinessActor.Handle`（Command Path）只回傳「成功/失敗 + 錯誤碼」，**唯一**夾帶 `Payload`（當前餘額）的路徑是**正常成功的寫入**。任何需要餘額的 Client，必須走獨立的 Query Path，不得依賴 Handle 回應。

**核心規則**：

| Handle 回傳路徑 | `Success` | `Payload` |
|---|---|---|
| 冪等重打（TxID 已在 txCache） | `true` | **空** |
| 餘額不足 | `false` | 空 |
| Payload 格式錯誤 | `false` | 空 |
| Cassandra 寫入失敗 | `false` | 空 |
| 正常成功寫入 | `true` | `encodeBalance()`（本次結算後的確切餘額） |

**為什麼冪等路徑不回傳餘額：**

`txCache` 只存 `txId → struct{}{}`（是否存在），不存原始回應結果。若冪等路徑回傳 `b.encodeBalance()`（當前餘額），Gateway 可能收到兩次同一個 `TxId` 的回應，第一次是 50（交易當下），第二次是 200（現在），語義上矛盾，導致客戶端無法判斷哪次才是「這筆交易的結算結果」。

若要儲存原始結果（`map[string][]byte`），每筆 payload 8 bytes × 1000 筆快取，記憶體開銷不划算。

**設計原則**：Command 只回答「有沒有成功」，餘額屬於系統狀態（State），查詢狀態走 Read Path。

## 5. Event Store 選擇 Cassandra

**決策**：事件溯源 (Event Sourcing) 的不可變日誌 (Event Log) 儲存選用 Cassandra。Partition 策略明確定義為複合主鍵 `(TenantID, UID)`。

**背景與理由**：
- **極致的寫入效能**：Cassandra 的 LSM-Tree 架構讓寫入操作為純粹的追加 (Append-only)，沒有 B+ Tree 的頁面分裂與 Random I/O 問題，完美契合 Event Sourcing 的高頻寫入需求。
- **神速的依 `(TenantID, UID)` 查詢效率**：只要我們將 `(TenantID, UID)` 設為 Partition Key，拉取特定租戶下特定玩家紀錄時，Cassandra 就會精準命中該 Partition 進行循序讀取。因為資料在磁碟上實體連續，Select 出某一個 Actor 所有的 Event 快如閃電，完美滿足 Actor Rehydration (狀態重建) 的低延遲需求。
