# Deployment Specification: Local Kind Cluster

本文件定義使用 `kind` (Kubernetes in Docker) 在本地端建立完整 Kubernetes 叢集，並部署 Actor Cluster 的所有基礎設施（etcd, Cassandra）與 Actor Node 應用的底層技術決策與資源拓樸配置。

## 1. 架構拓樸與網路邊界 (Topology & Networking)

- **Cluster Provisioning**: 使用 `kind` 建立一個輕量級的多節點 (Multi-node) 本地 K8s 環境（1 Control-Plane, 2+ Worker Nodes），以利於本地端真實驗證跨節點的 etcd 槽位註冊 (Slot Mapping) 與 gRPC 互通性。
- **Network Resolution**: 
  - 基礎服務（etcd / Cassandra）利用 K8s 內建的 ClusterIP Service DNS（如 `etcd-client.default.svc.cluster.local`, `cassandra-headless.default.svc.cluster.local`）。
  - Actor Node 的 Service Discovery 不依賴 K8s Service 的 Load Balancing。Node Pod 啟動時必須透過 **K8s Downward API** 將 `status.podIP` 注入到環境變數 `POD_IP`。Node 內的 `resolveMyIP` 將以此實體 IP 作為本節點對外 (etcd) 廣播的尋址基礎。

## 2. 狀態依賴元件部署 (Stateful Backing Services)

為確保 Actor 模型能正確進行事件溯源 (Event Sourcing) 與拓樸選舉，我們必須在本地 K8s 提供以下有狀態服務：

### 2.1 etcd (Service Discovery & Topology)
- **類型**: 部署為單節點（或三節點，視本地資源而定）的 `StatefulSet` + `Headless Service`。
- **配置**:
  - 開放 2379 port 供 K8s 內部存取。
  - **避坑紀錄 (Caveat)**: etcd lease (預設 5s) 的存活狀態極度依賴網路穩定度，本地 `kind` 應避免賦予過低的資源配置導致 etcd 頻繁觸發 Leader Election 或 Timeout 造成腦裂。

### 2.2 Cassandra (Event Store)
- **類型**: 部署為單節點 `StatefulSet` + `Headless Service`，映像與 **`deploy/infra/cassandra.yaml` 一致**（目前為 `cassandra:latest`；本地可調 `MAX_HEAP_SIZE` 等 env）。
- **持久化**: 掛載本機 `kind` 節點內的 `hostPath` 或 K8s `emptyDir` (若接受重啟掉資料)，以模擬 LSM-Tree 的高效磁碟 Append 操作。
- **初始化 (Migration)**:
  - 透過 `Makefile` 提供的 `make init-db` 指令，手動連入 Cassandra 執行 `cqlsh` 建立 `wallet` Keyspace 與 `wallet_events`, `wallet_snapshots` Schema。
  - **效能考量**: 應調低本地 C* 的 `MAX_HEAP_SIZE` (如 512M) 並調整 commit log 寫入模式，以避免開發環境吃光 RAM。

## 3. Actor Node K8s 部署整合 (Actor Node Integration)

Actor Node 若要順利融入 K8s 環境，其 Manifest 必須滿足以下工程要求：

- **環境變數與參數（以 `deploy/node.yaml` 為準）**:
  - **`POD_IP`**：必須從 `fieldRef: fieldPath: status.podIP` 注入；`cmd/node` 的 `resolveMyIP` 優先序為 `--ip` → `POD_IP` → UDP 探測。
  - **etcd / Cassandra 位址**：透過 **container `args`** 傳入（與本機 `go run` 的 flag 一致），例如 `-etcd=etcd-client:2379`、`-cassandra=cassandra-0.cassandra-headless.default.svc.cluster.local:9042`；**不**另設 `CASSANDRA_HOST` / `ETCD_ENDPOINTS` 等額外 env（避免文檔與 manifest 漂移）。
- **優雅停機 (Graceful Shutdown)**:
  - Pod 的 `terminationGracePeriodSeconds` 與 **`deploy/node.yaml` 一致**（目前為 **45s**）。
  - 根據 `#04_go_guidelines` 的分段退役策略，Node 在收到 `SIGTERM` 時，將依序與 etcd 發起 `Deregister`，並停止 TCP Server 接收新流量，隨後排空內部 MPSC 進行 Cassandra flush 操作。K8s 強制發送 SIGKILL 前必須保留足夠時間讓記憶體內的 Buffer 安全落盤。
- **連接埠 (Ports)**:
  - Pod 明確曝露 `50051` Port 作為 gRPC 傳輸層。

## 4. 自動化建置流與操作指南 (Bootstrap Workflow & Operations)

我們已經將所有冗長的 `kubectl` 與 `docker` 原生指令封裝至專案根目錄的 `Makefile` 中。開發者日常操作與壓測的標準作業流程 (SOP) 如下：

### 階段一：架設地基 (Infrastructure)
1. **`make kind-up`**:
   - **用途**: 根據 `deploy/infra/kind-config.yaml` 啟動包含 1-master 與 2-worker 的本地 K8s 虛擬叢集。這是一切部署的前置基底。
2. **`make deploy-infra`**:
   - **用途**: 將 etcd 與 Cassandra 啟動。
   - **確認方式**: 執行 `kubectl get pods -w`，等待這兩個元件狀態皆轉為 `Running`。
   - **初始化 DB**: 待 Cassandra 啟動完畢後，執行 `make init-db` 建立 Schema。

### 階段二：單機極速開發 (Hybrid Mode)
如果只想在 Mac 上用 Goland/VSCode 寫 Go code 測邏輯：
3. **`make port-forward`**:
   - **用途**: 會在背景啟動 K8s 內網穿透，將 K8s 內的 `etcd (2379)` 與 `Cassandra (9042)` 掛載到 Mac 本機 `127.0.0.1`。
   - **操作**: 開著此 Terminal 不要關（可按 Ctrl+C 中止），另開新視窗直接執行 `go run cmd/node/main.go` 即可享有原生 Debug 的無敵快感。

### 階段三：實兵跨節點壓測 (Distributed Load Test)
準備要挑戰高 TPS 與測試 `(TenantID, UID)` 的 etcd 路由（**`deploy/node.yaml` 預設 `replicas: 2`**，與下述兩台節點敘述一致；若要 3 台可改 replicas 或另擴展）：

4. **`make images`** 或 **`make docker-build`**:
   - **`make images`**：只在本機 Docker 建出 `actor:latest`（node）與 `actor-client:latest`（client），**不**載入 Kind。
   - **`make docker-build`**：等同 `images` + 對叢集 **`actor-cluster` 執行 `kind load`**，並執行 **`make verify-kind-images`**（在節點上 `crictl` 可見兩顆 image 即為載入成功）。

5. **`make deploy-node`**:
   - **用途**: `kubectl apply -f deploy/node.yaml`；**若映像 tag 未變（如仍為 `actor:latest`）**，`apply` 常顯示 `unchanged`，**不會**換掉已跑舊二進位的 Pod。改完 code 已 `kind load` 新層後，需 **`make redeploy-node`** 或一條龍 **`make refresh-nodes`**（= `docker-build` + `redeploy-node`）強制滾動重啟。

6. **`make load-test`**:
   - **用途**: 啟動大屠殺大砲 `cmd/client`，它會在 K8s 內網掛單執行。
   - **觀測數據**: 使用 `kubectl logs -l app=load-generator -f` 去看無鎖 Reporter 吐出的即時 TPS 跑馬燈。

### 階段四：關門打掃
7. **`make clean`**:
   - **用途**: 直接刪除整個 `kind` 叢集與其所有掛載硬碟，完全不留垃圾，下一秒重新 `make kind-up` 就是全新世界。

## 5. 工程與效能 Trade-off 分析

- **儲存層持久度 (Durability vs Convenience)**: 在本地 `kind` 環境中，Cassandra 的資料卷通常隨 Container / Pod 生命週期而消逝（使用 `emptyDir`）。這對於強調不可變日誌的 Event Sourcing 雖無法跨重啟保留資料，但極大化本地測試的啟動速度，且透過 `InitContainer` 補齊 Schema，已能涵蓋 99% 的本地開發情境。
- **多節點測試 (Multi-Node Testing)**: 使用 deployment 將 `appNode` 擴充至 replicas=3，可真實驗證 client gateway 或發送端如何讀取到不同 POD_IP 的 1024 槽位 hash mapped，並檢查是否在不同 IP 間發生正確的不重複派送。

## 6. 分散式壓測拓樸 (Distributed Load Testing)

因 Client 端必須實作依賴 etcd 獲取 1024 槽位的特製路由，且須負責請求的包裝 (Batching) 才能達到極限百萬 TPS 吞吐，因此**嚴格禁止使用 `ghz` 等泛用 gRPC 工具硬打單一 K8s Service**，這將導致無法正確根據 `(TenantID, UID)` 命中目標節點，引發無效拒絕。

壓測應採用以下拓樸與自定義元件：

- **自製壓測引擎 (`cmd/client`)**: 掛載 `pkg/discovery.EtcdResolver`（`TopologyResolver`）經**同套件**之 `GetNodeIP` 查表，決定目標節點，再依 `BatchRequest` 協定送出；細節見 `07_client_spec.md`。
- **內網穿透與直連**: 預設的 Mac 主機無法直接對 `kind` 內部的 `POD_IP` (`10.244.x.x` 網段) 發起路由。因此，為了真實壓測「跨節點路由直連」，`cmd/client` **必須打包為 Container 並以 `Job` 形式部署於 K8s 叢集內部**。
- **壓測流程定義**:
  1. K8s 控制台啟動至少 2 檯以上的 Actor Node (`replicas=2+`)。
  2. 部署壓測 `Job` (`cmd/client`)，其與 etcd 獲取當下的集群路由表。
  3. `Job` 在內部生成極高併發的交易，打包成巨型 Batch，精準地往不同的 `POD_IP` 傾瀉掃射。
  4. 透過 Log 或 Prometheus 等監控，捕捉真實 Actor Cluster 負載平衡下的延遲數據與硬體極限。
