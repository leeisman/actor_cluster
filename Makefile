.PHONY: kind-up deploy-infra init-db port-forward images verify-kind-images docker-build deploy-node redeploy-node refresh-nodes deploy-client load-test clean

CLUSTER_NAME=actor-cluster
IMAGE_NAME=actor:latest
CLIENT_IMAGE=actor-client:latest

# 設 NOCACHE=1 則 docker build 加 --no-cache（不依賴層快取，排除疑難或 CI 全量重編）
DOCKER_BUILD_XARGS :=
ifeq ($(NOCACHE),1)
DOCKER_BUILD_XARGS := --no-cache
endif

kind-up:
	@echo "Creating Multi-node Kind cluster..."
	kind create cluster --name $(CLUSTER_NAME) --config deploy/infra/kind-config.yaml
	@echo "Cluster created."

deploy-infra:
	@echo "Deploying etcd and cassandra..."
	kubectl apply -f deploy/infra/etcd.yaml
	kubectl apply -f deploy/infra/cassandra.yaml
	@echo "Wait for them to be ready: 'kubectl get pods -w'"
	@echo "Once cassandra is running, execute 'make init-db' to create tables."

init-db:
	@echo "Initializing Cassandra Schema..."
	kubectl exec -i cassandra-0 -- cqlsh -e "CREATE KEYSPACE IF NOT EXISTS wallet WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}; USE wallet; CREATE TABLE IF NOT EXISTS wallet_events (tenant_id int, uid bigint, version bigint, created_at timestamp, tx_id text, delta_amount bigint, payload blob, PRIMARY KEY ((tenant_id, uid), version)) WITH CLUSTERING ORDER BY (version ASC); CREATE TABLE IF NOT EXISTS wallet_snapshots (tenant_id int, uid bigint, balance bigint, last_version bigint, PRIMARY KEY ((tenant_id, uid)));"
	@echo "Database Initialized."

port-forward:
	@echo "Forwarding ports for Hybrid Development Mode..."
	@echo "Run 'go run cmd/node/main.go' smoothly in your local Mac!"
	@sh -c "\
	  kubectl port-forward svc/etcd-client 2379:2379 > /dev/null 2>&1 & \
	  kubectl port-forward svc/cassandra-headless 9042:9042 > /dev/null 2>&1 & \
	  echo 'Port-forwarding started in background. Press Ctrl+C to stop.' && \
	  wait \
	"

# 僅在本機 Docker 重建 node + client 映像（不需 Kind）。日常改程式後打這條即可。
# 快取：改動專案內檔案會使 COPY . . 層變更，之後的 go build 一般會重跑，映像內二進位會與源碼一致。
# 若有 .dockerignore 誤排源檔、或要強制全量重編：make images NOCACHE=1
# 未加 --progress：legacy builder（DOCKER_BUILDKIT=0）不支援該旗標；BuildKit 使用者可 export DOCKER_BUILDKIT=1。
images:
	@echo "Building $(IMAGE_NAME) (node) and $(CLIENT_IMAGE) (client)... $(if $(NOCACHE),(NOCACHE=1: no layer cache),)"
	docker build $(DOCKER_BUILD_XARGS) -t $(IMAGE_NAME) --target node -f deploy/build/Dockerfile .
	docker build $(DOCKER_BUILD_XARGS) -t $(CLIENT_IMAGE) --target client -f deploy/build/Dockerfile .
	@echo "Done. Check: docker images | grep -E 'actor|REPOSITORY'"

# 確認兩顆 image 出現在 $(CLUSTER_NAME) 各節點的 containerd（= kind load 成功載入叢集）
# 可單獨執行：make verify-kind-images
verify-kind-images:
	@if ! command -v kind >/dev/null 2>&1; then echo "kind not in PATH"; exit 1; fi
	@if ! kind get clusters 2>/dev/null | grep -Fqx "$(CLUSTER_NAME)"; then \
		echo "叢集 '$(CLUSTER_NAME)' 不存在。目前: $$(kind get clusters 2>/dev/null || true)"; \
		exit 1; \
	fi
	@echo "=== Local docker（本機應已 docker build 出此二 tag）==="
	@docker image ls 2>/dev/null | head -1
	@docker image ls 2>/dev/null | grep -E 'actor[[:space:]]|actor-client' || true
	@echo "=== 各 Kind 節點 crictl（應能 grep 到 library/actor、library/actor-client）==="
	@for n in $$(kind get nodes --name $(CLUSTER_NAME)); do \
		echo "-- $$n --"; \
		docker exec "$$n" crictl images 2>/dev/null | grep -E 'REPOSITORY|actor' || echo "  (無 actor* 行 — 先 make docker-build)"; \
	done
	@echo "=== verify-kind-images 結束 ==="

# 先 images，再把兩顆映像載入 Kind，最後在節點上檢查 crictl（叢集須已存在且名稱為 $(CLUSTER_NAME)）
docker-build: images
	@echo "Loading images into Kind ($(CLUSTER_NAME))..."
	kind load docker-image $(IMAGE_NAME) --name $(CLUSTER_NAME)
	kind load docker-image $(CLIENT_IMAGE) --name $(CLUSTER_NAME)
	@$(MAKE) --no-print-directory verify-kind-images

REPLICAS ?= 1

deploy-node:
	@echo "Deploying Actor Nodes..."
	kubectl apply -f deploy/node.yaml
	@echo "Scaling Node to $(REPLICAS) replicas..."
	kubectl scale deployment actor-node --replicas=$(REPLICAS)

# 在「已 kind load 新 actor:latest」之後用：同 tag 時 apply 不會觸發更新，需重啟 Deployment 讓 2 個 pod 用新 image 起新容器。
# 單跑：make redeploy-node
redeploy-node:
	@echo "Rolling restart deployment/actor-node (2 replicas) to pick up re-loaded image..."
	kubectl rollout restart deployment/actor-node
	kubectl rollout status deployment/actor-node --timeout=180s

# 一條龍：建 image、kind load、重啟 node pods（日常改完 node 就這條）
refresh-nodes: docker-build
	@$(MAKE) --no-print-directory redeploy-node

# 使壓測支援開多個 Terminal 並行，預設 JOB_ID=1，第二個視窗可用 make load-test JOB_ID=2
JOB_ID ?= 1

load-test:
	@echo "Spawning Client Load Generator Job-$(JOB_ID) inside K8s..."
	@kubectl delete job actor-load-generator-$(JOB_ID) --ignore-not-found
	@sed 's/name: actor-load-generator/name: actor-load-generator-$(JOB_ID)/; s/app: load-generator/app: load-generator-$(JOB_ID)/' deploy/client-job.yaml | kubectl apply -f -
	@echo "Waiting for Pod to spin up..."
	@sleep 2
	kubectl logs -l app=load-generator-$(JOB_ID) -f
	@echo "Check real-time TPS with: kubectl logs -l app=load-generator -f"

clean:
	@echo "Destroying Kind cluster..."
	kind delete cluster --name $(CLUSTER_NAME)
