package discovery

import (
	"context"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	defaultLeaseTTL      = 5
	registerRetryBackoff = 200 * time.Millisecond
	watchRetryBackoff    = 100 * time.Millisecond
)

// EtcdRegistry 負責將本機 node IP 以 ephemeral key 註冊到 etcd。
//
// Key schema:
//
//	{nodePrefix}{ip}
//
// 例如：
//
//	/actor_cluster/nodes/10.0.0.1:9000
//
// 註冊流程依賴 lease + keepalive：
//   - Register 先同步完成第一次 Grant + Put + KeepAlive，確保呼叫返回時 key 已存在。
//   - 之後由背景 goroutine 持續消費 keepalive stream。
//   - 若 keepalive stream 關閉或連線抖動，背景 goroutine 會自動重新 Grant/Put/KeepAlive。
//
// Data-race 說明：
//   - Registry 本身沒有在 goroutine 間共享可變狀態。
//   - 每次 lease 重建只在背景 goroutine 的堆疊內處理，不需要 mutex。
type EtcdRegistry struct {
	cli        *clientv3.Client
	nodePrefix string
	leaseTTL   int64
	value      string
}

// NewEtcdRegistry 建立一個新的 EtcdRegistry。
func NewEtcdRegistry(cli *clientv3.Client, nodePrefix string) *EtcdRegistry {
	return &EtcdRegistry{
		cli:        cli,
		nodePrefix: normalizeNodePrefix(nodePrefix),
		leaseTTL:   defaultLeaseTTL,
	}
}

// Register 會把指定 IP 註冊成帶 lease 的 ephemeral key，並在背景持續 keepalive。
//
// 行為保證：
//   - 方法返回 nil 時，表示第一次 etcd 註冊已經成功完成。
//   - 之後若 keepalive stream 中斷，背景 goroutine 會自動重建租約並重新註冊同一個 key。
//   - Register 不會在成功返回後阻塞當前 goroutine。
func (r *EtcdRegistry) Register(ctx context.Context, ip string) error {
	key := r.nodePrefix + ip

	keepAliveCh, err := r.establishRegistration(ctx, key)
	if err != nil {
		return err
	}

	go r.maintainRegistration(ctx, key, keepAliveCh)
	return nil
}

// Deregister 主動刪除本節點在 etcd 中的 ephemeral key。
//
// 雖然 etcd lease 到期後 key 會自動消失，但主動刪除可立即觸發其他節點的 watch 事件，
// 加速叢集拓樸更新（路由表切換），避免其他節點在 TTL 期間仍把流量導向已下線的節點。
// 建議在 graceful shutdown 序列的最前段呼叫，此時應傳入未取消的 context（如 context.Background()）。
func (r *EtcdRegistry) Deregister(ctx context.Context, ip string) error {
	key := r.nodePrefix + ip
	_, err := r.cli.Delete(ctx, key)
	return err
}

// Close 關閉 etcd client。
func (r *EtcdRegistry) Close() error {
	return r.cli.Close()
}

// EtcdResolver 實作 TopologyResolver，負責監聽 `/actor_cluster/nodes/` 之類的 node prefix，
// 從 etcd 攤平為「已排序節點位址」後 BuildRoutingTable 並 atomic 換入 tab；查表即 GetNodeIP(tab.Load(), …)。
//
// 設計重點：etcd 層只負責 KVs → sorted slice；槽位/預建表與 CoW 讀表皆同套件內 *Table 與 atomic.Pointer。
type EtcdResolver struct {
	tab atomic.Pointer[Table]

	cli        *clientv3.Client
	nodePrefix string
	slotOwner  SlotOwnerFunc
}

// NewEtcdResolver 建立一個新的 EtcdResolver。
// Resolver 本身不會主動啟動 watch；呼叫方需顯式呼叫 Watch(ctx)。
func NewEtcdResolver(cli *clientv3.Client, nodePrefix string) *EtcdResolver {
	return &EtcdResolver{
		cli:        cli,
		nodePrefix: normalizeNodePrefix(nodePrefix),
		slotOwner:  ModuloSlotOwner,
	}
}

// GetNodeIP：tab.Load 一次 + 同套件 GetNodeIP(*Table, …)（無鎖、無 defer、成功路徑無 alloc）。
func (r *EtcdResolver) GetNodeIP(tenantID int32, uid int64) (string, error) {
	return GetNodeIP(r.tab.Load(), tenantID, uid)
}

// Watch 啟動 topology 同步流程。
//
// 流程：
//  1. 先做一次 prefix GET，抓出所有存活 node key。
//  2. 抽出 IP，做字典序排序。
//  3. 建表並 tab.Store 換入。
//  4. 啟動背景 watch goroutine；任何 PUT/DELETE 事件都會觸發重抓快照。
//
// Failure semantics：
//   - 初始 GET 失敗：直接返回 error，因為呼叫方尚未有任何可用 routing table。
//   - watch channel 關閉或 watch error：sleep/backoff 後自動重建 watch。
//   - refreshSnapshot 失敗：保留舊表，不呼叫 Replace。
func (r *EtcdResolver) Watch(ctx context.Context) error {
	if err := r.refreshSnapshot(ctx); err != nil {
		return err
	}

	go r.watchLoop(ctx)
	return nil
}

// Close 關閉 resolver 持有的 etcd client。
func (r *EtcdResolver) Close() error {
	return r.cli.Close()
}

func (r *EtcdRegistry) maintainRegistration(ctx context.Context, key string, keepAliveCh <-chan *clientv3.LeaseKeepAliveResponse) {
	currentCh := keepAliveCh

	for {
		if !consumeKeepAlive(ctx, currentCh) {
			return
		}
		if !sleepWithContext(ctx, registerRetryBackoff) {
			return
		}

		nextCh, err := r.establishRegistration(ctx, key)
		if err != nil {
			return
		}
		currentCh = nextCh
	}
}

func (r *EtcdRegistry) establishRegistration(ctx context.Context, key string) (<-chan *clientv3.LeaseKeepAliveResponse, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		leaseResp, err := r.cli.Grant(ctx, r.leaseTTL)
		if err != nil {
			if !sleepWithContext(ctx, registerRetryBackoff) {
				return nil, ctx.Err()
			}
			continue
		}

		_, err = r.cli.Put(ctx, key, r.value, clientv3.WithLease(leaseResp.ID))
		if err != nil {
			if !sleepWithContext(ctx, registerRetryBackoff) {
				return nil, ctx.Err()
			}
			continue
		}

		keepAliveCh, err := r.cli.KeepAlive(ctx, leaseResp.ID)
		if err != nil {
			if !sleepWithContext(ctx, registerRetryBackoff) {
				return nil, ctx.Err()
			}
			continue
		}

		return keepAliveCh, nil
	}
}

func (r *EtcdResolver) watchLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		watchCh := r.cli.Watch(ctx, r.nodePrefix, clientv3.WithPrefix())
		for {
			select {
			case <-ctx.Done():
				return
			case resp, ok := <-watchCh:
				if !ok {
					if !sleepWithContext(ctx, watchRetryBackoff) {
						return
					}
					goto restartWatch
				}
				if resp.Err() != nil {
					if !sleepWithContext(ctx, watchRetryBackoff) {
						return
					}
					goto restartWatch
				}
				if hasNodeTopologyEvent(resp.Events) {
					if err := r.refreshSnapshot(ctx); err != nil {
						continue
					}
				}
			}
		}

	restartWatch:
		continue
	}
}

func (r *EtcdResolver) refreshSnapshot(ctx context.Context) error {
	resp, err := r.cli.Get(ctx, r.nodePrefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}

	sortedIPs := sortedNodeIPsFromKVs(r.nodePrefix, resp.Kvs)
	r.tab.Store(BuildRoutingTable(sortedIPs, r.slotOwner))
	return nil
}

func hasNodeTopologyEvent(events []*clientv3.Event) bool {
	for i := 0; i < len(events); i++ {
		switch events[i].Type {
		case clientv3.EventTypePut, clientv3.EventTypeDelete:
			return true
		}
	}
	return false
}

func sortedNodeIPsFromKVs(prefix string, kvs []*mvccpb.KeyValue) []string {
	if len(kvs) == 0 {
		return nil
	}

	ips := make([]string, 0, len(kvs))
	for i := 0; i < len(kvs); i++ {
		key := string(kvs[i].Key)
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		ip := strings.TrimPrefix(key, prefix)
		if ip == "" {
			continue
		}
		ips = append(ips, ip)
	}

	sort.Strings(ips)
	return ips
}

func normalizeNodePrefix(prefix string) string {
	trimmed := strings.TrimRight(prefix, "/")
	if trimmed == "" {
		return "/"
	}
	return trimmed + "/"
}

func consumeKeepAlive(ctx context.Context, keepAliveCh <-chan *clientv3.LeaseKeepAliveResponse) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case resp, ok := <-keepAliveCh:
			if !ok || resp == nil {
				return true
			}
		}
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
