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
- **類型**: 部署為單節點 `StatefulSet` + `Headless Service`，映像與 **`deploy/infra/cassandra.yaml` 一致**（目前固定為 `cassandra:4.1`；本地可調 `MAX_HEAP_SIZE` 等 env）。
- **持久化**: 掛載本機 `kind` 節點內的 `hostPath` 或 K8s `emptyDir` (若接受重啟掉資料)，以模擬 LSM-Tree 的高效磁碟 Append 操作。
- **初始化 (Migration)**:
  - 透過 `Makefile` 提供的 `make init-db` 指令，手動連入 Cassandra 執行 `cqlsh` 建立 `wallet` Keyspace 與 `wallet_events`, `wallet_snapshots` Schema。
  - **效能考量**: 本地 kind / Docker Desktop 以穩定為先，應調低本地 C* 的 `MAX_HEAP_SIZE` (如 512M) 與 `HEAP_NEWSIZE` (如 100M)，避免在開發環境出現過度吃 RAM 或 JVM 啟動不穩。

## 3. Actor Node K8s 部署整合 (Actor Node Integration)

Actor Node 若要順利融入 K8s 環境，其 Manifest 必須滿足以下工程要求：

- **環境變數與參數（以 `deploy/node.yaml` 為準）**:
  - **`POD_IP`**：必須從 `fieldRef: fieldPath: status.podIP` 注入；`cmd/node` 的 `resolveMyIP` 優先序為 `--ip` → `POD_IP` → UDP 探測。
  - **etcd / Cassandra 位址**：透過 **container `args`** 傳入（與本機 `go run` 的 flag 一致），例如 `-etcd=etcd-client:2379`、`-cassandra=cassandra-0.cassandra-headless.default.svc.cluster.local:9042`；**不**另設 `CASSANDRA_HOST` / `ETCD_ENDPOINTS` 等額外 env（避免文檔與 manifest 漂移）。
  - **metrics 位址**：Node 另外透過 **container `args`** 傳入 `-metrics=:9090`，提供 node-local `/metrics` HTTP endpoint。
- **優雅停機 (Graceful Shutdown)**:
  - Pod 的 `terminationGracePeriodSeconds` 與 **`deploy/node.yaml` 一致**（目前為 **45s**）。
  - 根據 `#04_go_guidelines` 的分段退役策略，Node 在收到 `SIGTERM` 時，將依序與 etcd 發起 `Deregister`，並停止 TCP Server 接收新流量，隨後排空內部 MPSC 進行 Cassandra flush 操作。K8s 強制發送 SIGKILL 前必須保留足夠時間讓記憶體內的 Buffer 安全落盤。
- **連接埠 (Ports)**:
  - Pod 明確曝露 `50051` Port 作為 gRPC 傳輸層。
  - Pod 額外曝露 `9090` Port 作為 node metrics endpoint。
- **Prometheus Pod Annotations**:
  - `deploy/node.yaml` 目前會標記：
    - `prometheus.io/scrape: "true"`
    - `prometheus.io/path: "/metrics"`
    - `prometheus.io/port: "9090"`
  - 讓 Prometheus 以 pod annotation discovery 直接抓取 node metrics，而不需額外建立專用 metrics Service。

## 4. 自動化建置流與操作指南 (Bootstrap Workflow & Operations)

我們已經將所有冗長的 `kubectl`、`kind` 與 `docker` 原生指令封裝至專案根目錄的 `Makefile` 中。現行標準流程如下。

### 4.1 狀態檢查

1. **`make status-kind`**：
   - **用途**：列出目前 kind cluster、nodes、pods、ingress 與 gateway 部署狀態。
   - **適用時機**：先確認現在缺的是 `ingress-nginx`、`actor-gateway`，還是整個 cluster 根本不存在。

### 4.2 重建 kind（必要時）

2. **`make recreate-kind`**：
   - **用途**：刪掉舊 cluster，再依 `deploy/infra/kind-config.yaml` 重建。
   - **為什麼需要**：`deploy/infra/kind-config.yaml` 現在已要求 control-plane 具備 `80/443` host port mapping，供 ingress 對外暴露 web gateway。若 cluster 是在這次設定之前建立的，**舊 cluster 不會自動補上 port mapping**，必須重建。

### 4.3 完整一條龍流程

3. **`make bootstrap-all`**：
   - **用途**：從零建立完整本地環境。
   - **內含順序**：
     1. `recreate-kind`
     2. `deploy-infra`
     3. `wait-infra`
     4. `init-db`
     5. `install-ingress`
     6. `docker-build`
     7. `deploy-node`
     8. `deploy-gateway`
     9. `status-kind`

這是目前最推薦的「統一步驟」。

### 4.4 Gateway 路徑

4. **`make install-ingress`**：
   - **用途**：安裝 `ingress-nginx` controller（kind provider manifest）。
   - **結果**：叢集可接收 `Ingress` 資源，將 `http://actor-cluster.localhost/` 路由到 cluster 內的 web gateway service。
   - **固定規則**：安裝後 Makefile 會立即把 `ingress-nginx-controller` 固定到 `actor-cluster-control-plane`。原因是本專案的 `80/443` host port mapping 只綁在 kind control-plane container；若 controller 被 scheduler 放到 worker，瀏覽器雖然可以連到本機 port 80，但實際上不會命中 ingress controller。

5. **`make deploy-gateway`**：
   - **用途**：部署 `actor-gateway` 的 `Deployment + Service + Ingress`。
   - **對外入口**：`http://actor-cluster.localhost/`

6. **`make redeploy-gateway`**：
   - **用途**：在已經 `kind load` 新 `actor-client:latest` 之後，滾動重啟 gateway deployment 讓新 binary 生效。

7. **`make bootstrap-gateway`**：
   - **用途**：對已存在的 cluster 做 gateway 路徑的一條龍部署。
   - **內含順序**：
     1. `install-ingress`
     2. `docker-build`
     3. `deploy-gateway`

### 4.5 Node 與壓測路徑

8. **`make deploy-infra`**：
   - **用途**：啟動 etcd 與 Cassandra。
   - **確認方式**：可用 `kubectl get pods -w` 或 `make status-kind`。

9. **`make init-db`**：
   - **用途**：建立 `wallet` keyspace 與 `wallet_events`, `wallet_snapshots` schema。
   - **穩定性**：Makefile 會先等待 Cassandra 的 native transport 可被 `cqlsh` 連上，再真正執行 schema 初始化，避免 Cassandra 剛完成 rollout 但 `9042` 尚未開始接受連線時的初始化失敗。

9.1 **`make reset-db`**：
   - **用途**：只清空本地 Cassandra 中的壓測資料，不重建 kind cluster。
   - **行為**：
     - 先等待 Cassandra 的 native transport 可被 `cqlsh` 連上。
     - 確保 `wallet` keyspace 存在。
     - 對 `wallet_events`、`wallet_snapshots` 執行 `TRUNCATE`。
     - 再執行 `nodetool clearsnapshot wallet`，減少 `TRUNCATE` 後舊 snapshot 仍占用本地磁碟的情況。
   - **適用情境**：
     - 本地反覆跑 `make load-test` 後，想清掉累積的 event / snapshot 資料。
     - 想保留既有 cluster、ingress、gateway、monitoring，不想每次都 `make clean`。
   - **限制**：
     - 這條會明顯比單純 `TRUNCATE` 更接近「清乾淨」，但仍不等於整個 kind cluster 重建。
     - 若 Docker / kind 底層 storage 已嚴重碎裂或被其他資料吃滿，仍可能需要 `make clean` + `make bootstrap-all`。

10. **`make images`** / **`make docker-build`**：
   - **`make images`**：只在本機 Docker 建出 `actor:latest`（node）與 `actor-client:latest`（client），**不**載入 kind。
   - **`make docker-build`**：等同 `images` + 對叢集 **`actor-cluster` 執行 `kind load`**，並執行 **`make verify-kind-images`**。

11. **`make deploy-node`**：
   - **用途**：部署 Actor Node。
   - **補充**：
     - 這條會先 `kubectl apply -f deploy/node.yaml`，因此適合 manifest / args / annotations 有變動時使用。
     - 若 image tag 未變（仍為 `actor:latest`），改完 code 後需額外執行 **`make redeploy-node`** 或 **`make refresh-nodes`** 才會真的換成新 binary。

11.1 **`make redeploy-node`**：
   - **用途**：在 kind 內已 load 新 image 後，重啟 `actor-node` deployment。
   - **現行行為**：
     - 會先重新 `apply deploy/node.yaml`
     - 再 `rollout restart deployment/actor-node`
   - 這樣即使 `deploy/node.yaml` 的 args / ports / annotations 有變更，也不會只重啟舊 manifest。

12. **`make load-test`**：
   - **用途**：以 K8s Job 方式啟動 `cmd/client stress ...`，做真實內網壓測。
   - **支援參數**：可從 Makefile 傳入 `TPS`、`CONCURRENCY`、`BATCH`、`FLUSH_DELAY`、`DURATION`、`DRAIN_WINDOW`、`UID_MIN`、`UID_MAX`，對應 `cmd/client stress` 的 CLI flags。
   - **補充**：
     - `BATCH` 直接對應 `-batch`，用來控制單次送往 node 的最大 envelope 批次大小。
     - `FLUSH_DELAY` 直接對應 `-flush-delay`，用來控制未滿 batch 最多等待多久就強制送出。

12.1 **`make stop-load-test` / `make stop-all-load-tests`**：
   - **用途**：停止 cluster 內仍在執行的 load-generator Job。
   - **背景**：
     - `make load-test` 啟動的是 Kubernetes Job，即使本地 terminal session 結束，Job 仍可能繼續執行。
   - **指令**：
     - `make stop-load-test JOB_ID=<n>`：刪除單一 `actor-load-generator-<n>` Job 與其 Pod。
     - `make stop-all-load-tests`：刪除所有 `actor-load-generator-*` Job 與其對應 Pod。

### 4.6 單機極速開發 (Hybrid Mode)

13. **`make port-forward`**：
   - **用途**：將 K8s 內的 etcd / Cassandra 掛到本機 `127.0.0.1`，方便直接 `go run ./cmd/node` 或 `go run ./cmd/client serve`。

### 4.7 清理

14. **`make clean`**：
   - **用途**：刪除整個 kind cluster。

14.1 **本地 Cassandra 容量管理建議**：
   - 本專案的本地 Cassandra 主要用於開發與壓測，不是長期持久化環境。
   - 若反覆壓測造成資料量膨脹，應優先考慮 **`make reset-db`** 清空測試資料。
   - 只有在需要完全重建 cluster 狀態時，再使用 **`make clean`**。

### 4.8 Monitoring Baseline（Prometheus / Grafana）

15. **目標**：
   - 在本地 `kind` 環境先建立一套最小可用的觀測平台，讓開發者能看到：
     - K8s 物件狀態（Pod Ready / Restart）
     - Node CPU / Memory
     - Prometheus 原始查詢
     - Grafana 基礎圖表
   - **本階段只負責部署觀測平台，不預設承諾大量 application-level metrics。**

16. **部署元件（第一版）**：
   - `Prometheus`
   - `Grafana`
   - `kube-state-metrics`
   - `node-exporter`
   - 帶有 `prometheus.io/scrape=true` 的 annotated pods（目前包含 `actor-node`）

17. **部署位置**：
   - 建議獨立使用 `monitoring` namespace，避免與 `default` 業務元件混在一起。
   - Prometheus 與 Grafana 均透過 `deploy/monitoring/` 下的 manifest 維護。
   - **Manifest 檔案（第一版）**：
     - `namespace.yaml`
     - `prometheus-rbac.yaml`（Prometheus `ServiceAccount` + `ClusterRole` + `ClusterRoleBinding`）
     - `prometheus-config.yaml`（Prometheus `ConfigMap`：`prometheus.yml`）
     - `prometheus.yaml`（Prometheus `Deployment` + `Service`）
     - `kube-state-metrics.yaml`（`ServiceAccount`、`ClusterRole`、`ClusterRoleBinding`、`Deployment`、`Service`）
     - `node-exporter.yaml`（`DaemonSet` + `Service`）
     - `grafana.yaml`（Grafana datasource provisioning `ConfigMap`、dashboard provider `ConfigMap`、built-in dashboard `ConfigMap`、`Deployment`、`Service`）
     - `ingress.yaml`（**`Ingress` 名稱：`monitoring-ui`**；`grafana.localhost` / `prometheus.localhost`，**需已執行** `make install-ingress`；與舊版 `Ingress/monitoring` 二選一，`deploy-monitoring` 會刪除後者以免 nginx admission 重複 host）

18. **入口設計**：
   - **Grafana / Prometheus 對外訪問**可採兩種模式：
     - **Ingress 模式**：以 host-based routing 綁定本機 `80`，例如：
       - `http://grafana.localhost/`
       - `http://prometheus.localhost/`
     - **Port-forward 模式**：作為 ingress 以外的 debug 後門，例如：
       - `127.0.0.1:3000` -> Grafana
       - `127.0.0.1:9090` -> Prometheus

19. **Makefile 建議入口**（已實作於專案根目錄 `Makefile`）：
   - `make deploy-monitoring`
     - 依序 `kubectl apply` 上述 manifest（`kube-state-metrics` 前有 **selector 遷移**：若現有 `Deployment/kube-state-metrics` 的 `spec.selector` 不是 `app=kube-state-metrics`，則**只刪除該 Deployment** 再 apply，避免 immutable selector 造成 apply 失敗；`ingress.yaml` 前會 **`kubectl delete ingress/monitoring`**（`--ignore-not-found`）以免與 `monitoring-ui` 重複綁定相同 host）。
     - 由於 `prometheus-config.yaml` / Grafana datasource provisioning / dashboard provisioning 都來自 ConfigMap，這條會額外 `rollout restart`：
       - `deployment/prometheus`
       - `deployment/grafana`
     - 最後等待 `prometheus`、`kube-state-metrics`、`grafana` rollout 與 `node-exporter` DaemonSet 就緒，並列印 `make monitoring-urls` 的提示。
   - `make redeploy-monitoring`
     - 與 `deploy-monitoring` 相同（idempotent 重套 manifest）。
   - `make monitoring-port-forward`
     - 背景啟動 `grafana:3000`、`prometheus:9090` 的 port-forward（適合未走 Ingress 時）。
   - `make monitoring-urls`
     - 列出 Ingress 與 port-forward 兩種訪問方式。

20. **與 `bootstrap-all` 的關係**：
   - `make bootstrap-all` **不包含** monitoring；叢集起來後若要觀測底座，請另執行 `make install-ingress`（若要走 Ingress）與 `make deploy-monitoring`。

21. **Grafana 內建 dashboard（目前已實作一份最小版）**：
   - Grafana 會自動 provision `Actor Cluster` folder，其中包含 `Actor and Persistence Avg` dashboard。
   - 目前內建 panel：
     - `Handle vs Persistence Calls Per Second`
     - `Handle vs Persistence Avg (1m, ms)`
     - `Handle - Persistence Gap (1m, ms)`
     - `Actor Mailbox Pending`
   - dashboard 原始 JSON 追蹤於：
     - `deploy/monitoring/actor-persistence-dashboard.json`
   - 這份 dashboard 的定位是：
     - 用最少幾個 application metrics，快速回答「actor handle 成本是否主要由 persistence write 主導」。

22. **這一版刻意不做的事情**：
   - 不先引入大量 `cmd/client` / `internal/node` / `pkg/actor` 的 Prometheus 埋點
   - 不先定義 AI bottleneck analysis
   - 不先做大量、產品化的 dashboard catalog；目前只提供一份最小診斷 dashboard
   - 不先做 eBPF / deep tracing

23. **工程取捨**：
   - 先把 Prometheus / Grafana 部署進來，目的是建立觀測底座。
   - 真正的 application metrics 必須等我們先釐清：
     - 要回答什麼問題
     - 哪些指標值得長期維護
   - 否則容易在代碼內埋入過量、低價值且高維護成本的 metrics。

24. **目前已存在的最小 application metric**：
   - 雖然 baseline 原則是不先大量主導 app 埋點，但目前 `actor-node` 已額外提供：
     - `node_actor_mailbox_pending`
   - 這個值代表 **單一 node process 內所有 actor mailbox 的 pending envelope 總量**，用來觀察壓測下 actor backlog 是否升高、以及停止送流量後是否回落。
   - `cmd/node` 目前另有 `--mailbox-limit`（預設 `1500000`）作為第一版 node 級 overload gate；
     當 mailbox backlog 已達上限時，node 會直接回 `ERR_NODE_OVERLOADED`，避免無界堆積最終導致 OOM。

### 4.9 避坑紀錄

1. **舊 cluster 必須重建**：
   - `deploy/infra/kind-config.yaml` 更新後，既有 kind cluster 不會自動得到新的 `80/443` host port mapping。
   - 只要本專案的 ingress 路徑有調整，就應優先使用 `make recreate-kind` 或 `make bootstrap-all`。

2. **ingress controller 不能隨機排到 worker**：
   - 這次實際踩到的症狀是：
     - `kubectl get ingress` 顯示正常
     - `actor-gateway` service 在 cluster 內可正常回應
     - 但本機 `curl http://actor-cluster.localhost/` 只得到 `Empty reply from server`
   - 根因是 ingress controller 在 worker，而本機 port 80 mapping 只接到 control-plane。

3. **Cassandra rollout 完成不等於 cqlsh 已可用**：
   - `kubectl rollout status statefulset/cassandra` 成功後，native transport 仍可能需要額外數秒才會真正接受 `cqlsh` 連線。

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
