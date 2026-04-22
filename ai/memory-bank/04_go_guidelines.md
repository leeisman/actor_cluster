# Go Performance Guidelines (效能開發鐵則)

開發此高併發 Actor 核心時，必須嚴格遵守以下效能開發準則，所有代碼均需經得起 CPU Profile 與 Memory Profile 的考驗。

## 1. 記憶體管理與逃逸分析 (Escape Analysis)

- **避免不必要的指標傳遞**：除需要修改內部狀態的 Struct 之外，輕量級 Struct (如簡單 Event 封裝) 盡量優先傳值 (Value Semantics)，避免逃逸到 Heap 觸發 GC。
- **強制使用 `sync.Pool`**：對於生命週期短且高頻建立的物件 (例如：Message Envelope、Batch Request)，全面使用 `sync.Pool` 回收覆用，達到 Zero Allocation 的目標。
- **預先配置容量 (Pre-allocation)**：任何 Slice 與 Map，在初始化時只要能預估數量，必須給定 Capacity 容量 (如 `make([]int, 0, 1024)`)，防止自動重分配與搬移開銷。
- **Zero-Allocation 陣列覆用**：需要清空 Slice 時，請使用 `slice = slice[:0]`。這會將長度歸零但保留底層 Capacity，配合迴圈能達到近乎零 GC 開銷的無限覆用（例如在 Batching I/O 階段）。

## 2. 嚴格禁止熱點 `defer`

- **Hot-path 禁令**：在每一微秒都錙銖必較的熱點代碼 (如 Actor 的 `Receive` 迴圈、MPSC 中拉取資料、Lock 的開關) 中，**禁止**使用 `defer`。
- **手動解鎖**：改為顯式呼叫 `Unlock()` 或資源釋放。Go 的 defer 雖然在 1.14 後優化為 open-coded，但在極限併發下仍會產生堆疊操作的額外延遲與潛在的逃逸問題。

## 3. 無鎖與原子操作 (Atomic Operations)

- **優先使用 `sync/atomic`**：例如計算 Queue 的 Head/Tail 指標、狀態機的無鎖變更、以及計數器，全面使用 `atomic.AddInt64`, `atomic.CompareAndSwapInt64` 等指令。
- **消除 False Sharing**：對於高頻變更的 padding 欄位，需注意 CPU Cache Line (通常為 64 Bytes)，手動填充 Padding (`_ [7]uint64`) 以避免不同 CPU Core 同時失效快取的問題。

## 4. 併發與 Goroutine 粒度

- **Goroutine 不隨意增生**：絕不替每一個 Request 生成一個 Goroutine。系統必須採用一套固定 Size 的 Worker Pool 或依附於 Actor 本身的長駐 Goroutine 來處理。
- **狀態隔離**：每個 Actor 內部必須保證是單執行緒 (Single-threaded) 執行邏輯，Actor 內部修改狀態絕對不需要任何 Mutex 鎖。所有外界交互只能透過發送 Message (經由 MPSC Queue) 進行。

## 5. 高效寫入與讀取

- **String 與 Byte 的零拷貝轉換**：在特定的底層解析與儲存元件中，合理採用 `unsafe.Pointer` 來進行 `string` 和 `[]byte` 間的零拷貝 (Zero-Copy) 轉換。
- **避免 Reflection**：禁止在核心路徑上使用標準庫的 `reflect`。所有的序列化操作需依賴提前編譯生成的代碼 (如 Protoc-gen-go)。

---



## 6. 錯誤處理與 Graceful Shutdown

- **使用 `panic(err)` 取代 `log.Fatalf`**：在 `main()` 函式中，若遇到啟動失敗（如資料庫連線失敗、Port 佔用），絕對禁止使用 `log.Fatalf` 或 `os.Exit`。因為這會導致 `main` 內的 `defer` (如 `store.Close()`) 無法執行。應改用 `panic(err)` 觸發 Stack Unwinding，確保資源優雅釋放。

- **Pipeline 分段關閉：由上游往下游順流切斷**：系統的資料流是一條有方向的 Pipeline，關閉時必須從最上游往最下游依序切斷，確保每個階段都能把手上的資料處理完再退出，不丟任何一筆結果。

  **Pipeline 全貌**：
  ```
  gRPC TCP → HandleBatch → MPSC mailbox → [Actor goroutine] → resultCh → aggregatorLoop → gRPC stream
                                             ↓（消費 mailbox）
                                          寫入 Cassandra
                                             ↓（產生 Result）
  ```
  Actor 在 pipeline 中是一個**轉換節點**，同時扮演消費者（消費 MPSC mailbox）與生產者（產生 Result 到 resultCh）兩個角色，不能用簡單的「生產者/消費者」二元來描述。

  **正確的三段式關閉順序**：
  1. **關上游入口（gRPC TCP）**：呼叫 `grpcServer.GracefulStop()`，等所有 in-flight `HandleBatch` 呼叫完成。此後 MPSC mailbox 不再有新信件加入。
  2. **排空 Actor（中間轉換節點）**：呼叫 `bActor.core.Stop()`，讓 Actor goroutine 排空 mailbox、寫完 Cassandra、把最後的 Result 寫入 `resultCh`。此時 `aggregatorLoop` 必須保持存活，否則 `resultCh` buffer 塞滿後，Actor goroutine 永久阻塞在 `Emit`，Stop() 永遠不返回（Deadlock）。
  3. **關閉下游（aggregatorLoop）**：Actor 全死後 `resultCh` 不再有新資料，才 `close(n.done)`，讓 aggregatorLoop 做最後一次 `flush()` 送出 gRPC 結果後退出。

  **反面教材**：若先 `close(n.done)` 再停 Actor，在高流量 SIGTERM 時 `resultCh` buffer（再大也有限）一旦塞滿，所有 Actor goroutine 無聲無息地 Hang 在 `Emit`，Process 永遠不退出，且不會有任何 Error Log。這類 Bug 在低流量測試環境下永遠不會觸發。

- **關機雪崩效應（Shutdown Thundering Herd）**：絕不在所有 Actor 退出時強制寫快照（`SetShutdownFunc(takeSnapshotSync)`）。若節點有 10 萬個活躍 Actor，Node.Stop() 會在同一秒觸發 10 萬次並發 Cassandra UPSERT，導致 DB CPU 飆高，節點關機時間拖長後被 K8s SIGKILL 強殺。正確做法：**讓 Actor 安靜快速地死去**。未快照的 Delta Events 早已安全落地於 `wallet_events`，rehydrate 成本由接手節點分散承擔。快照只在「每 N 筆事件」的週期路徑上非同步觸發。

## 7. 避免過度設計 (Over-engineering)

- **微秒級鎖競爭 vs. Heap Allocation**：在設計背景掃描任務（如 Passivation）時，若鎖的持有時間極短（例如遍歷數千個 map 元素並進行簡單運算，耗時 < 10 微秒），應直接使用 `Lock`。避免為了「縮短鎖持有時間」而採用 RCU (Read-Copy-Update) 模式去建立暫存 Slice 收集名單，這會導致不必要的 Heap Allocation 與 GC 壓力。
- **使用 `goto` 解決微小機率的 Race Condition**：在無鎖或細粒度鎖的設計中，常會遇到「檢查後失效 (Check-Then-Act)」的 Race Condition。此時應使用 `goto retry` 跳回檢查點重試，這比使用遞迴 (Recursion) 效能更好，且不會增加 Call Stack。

## 8. Channel 傳遞與指標

- **提早 Allocation 以減少拷貝**：若 Channel 傳遞的資料最終必須轉換為某個會逃逸到 Heap 的結構（例如 gRPC 的 Protobuf Response 指標），應在資料產生的源頭（Producer）就直接 Allocate 該指標結構並傳入 Channel (`chan *T`)。這能避免 Value Type (`chan T`) 在傳遞過程中的多次 Struct 拷貝，並讓 Consumer 端達到零轉換成本。

## 9. 分散式一致性的輸入排序

- **去中心化路由的必要前提**：當所有節點各自在本地記憶體用演算法建立 routing table 時，輸入的 IP 列表必須先做字典序排序 (`sort.Strings`)。任何一台機器拿到的 etcd key 順序可能不同，少了排序這一步，modulo / consistent hashing 算出的 owner 就會各說各話，造成分散式腦裂。

- **演算法單一套件**：`SlotOf`、預建 `[1024]string` 表、預設 `ModuloSlotOwner` 與 **`GetNodeIP(*Table,…)`**（`EtcdResolver` 內以 `atomic.Pointer[Table]` 持表）只允許實作在 **`pkg/discovery`**；禁止在 `cmd/*`、`internal/node` 等處複製雜湊或魔數，避免行為與文檔漂移。

## 10. 全域 Map 鎖競爭防護 (Sharded Map)

- **避免 `sync.Map` 成為萬點連線瓶頸**：在模擬 Gateway 狀態或高併發的 WebSocket Request-ID 回調 (Correlation Callback) 時，單一的 `sync.Map` 或 `Mutex` 會面臨嚴重的鎖競爭 (Lock Contention)。對此應實作 **「分片鎖 (Sharded Map)」**。
- **實作方式**：宣告固定長度的陣列（如 `[256]struct{ sync.RWMutex; map }`），透過對 Key 取模（如 `ReqID % 256`）將寫入/讀取的操作分配到不同分片，這能將單點鎖的壓力稀釋掉 256 倍，是突破單機千萬 TPS 的必備技巧。
