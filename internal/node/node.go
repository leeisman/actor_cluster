package node

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/frankieli/actor_cluster/pkg/actor"
	"github.com/frankieli/actor_cluster/pkg/discovery"
	"github.com/frankieli/actor_cluster/pkg/persistence"
	"github.com/frankieli/actor_cluster/pkg/remote"
	pb "github.com/frankieli/actor_cluster/pkg/remote/pb"
)

// resultRouteKey 將一筆請求關聯到建立該請求之 gRPC stream 的 BatchSender。
// 多 client 併發時不可使用單一 n.sender 指標（最後一條 stream 覆寫導致回包錯流）。
// 系統契約：request_id 由上游入口生成，於整個叢集範圍內全域唯一，而非僅單一 stream 內唯一；
// 因此以 (tenant, uid, request_id) 作為 reply route key 不會在不同 stream 間碰撞。
// 此鍵在 Send 到 Actor 前註冊、Emit 或 drain 時刪除。
type resultRouteKey struct {
	tenant int32
	uid    int64
	req    uint64
}

type queuedResult struct {
	res   *pb.RemoteResult
	reply remote.BatchSender
}

const defaultMailboxLimit uint64 = 1500000

type Config struct {
	MailboxLimit uint64

	// mailboxPendingFn 僅供測試覆寫；正式環境預設為 actor.ProcessMailboxPending。
	mailboxPendingFn func() uint64
}

// ActorKey uniquely identifies an actor.
type ActorKey struct {
	TenantID int32
	UID      int64
}

// shardIndex returns the shard bucket [0, 256) this key belongs to.
// XOR of TenantID and lower 32 bits of UID distributes keys evenly across shards.
func (k ActorKey) shardIndex() uint64 {
	return uint64(k.TenantID^int32(k.UID)) % 256
}

// Node is the business runtime that implements remote.BatchHandler.
//
// Ownership model（防腦裂）：
//   - Node 啟動時注入一個 discovery.TopologyResolver 與自己的 myIP。
//   - 每筆 Envelope 在進入 Shard 之前，先呼叫 resolver.GetNodeIP 確認自己是 owner。
//   - 若非 owner（路由表已更新），回傳 WRONG_NODE 錯誤並執行 forceKillLocalActor，
//     讓已過時的 Actor 從記憶體清除，下一次路由到正確 Node 時重新 rehydrate。
type Node struct {
	shards [256]struct {
		sync.RWMutex
		actors map[ActorKey]*BusinessActor
	}

	store persistence.Store

	// resolver 用於 O(1) 所有權查詢，nil 時跳過防護（單節點測試用）。
	resolver discovery.TopologyResolver
	// myIP 是本機對外的 host:port，用於和 resolver 回傳值比對。
	myIP string

	// replyRoutes：Send 到 Actor 前 (tenant,uid,request_id) -> 該筆所屬 gRPC stream 的 BatchSender。
	replyRoutes sync.Map // map[resultRouteKey]remote.BatchSender

	// resultCh 是 MPSC 結果佇列：多個 Actor goroutine 寫入，單一 aggregatorLoop 讀取；每筆帶有回傳目標 stream。
	resultCh chan queuedResult

	// mailboxLimit 是 node 級 backlog 上限；超過時直接拒絕新請求，不再 enqueue 到 actor mailbox。
	mailboxLimit   uint64
	mailboxPending func() uint64

	// done 信號所有背景 loop 在 Stop 時退出。
	done chan struct{}
	wg   sync.WaitGroup
}

// NewNode 建構 Node 並啟動背景 goroutine。
//
// resolver 傳 nil 可跳過所有權防護（適用於單節點或測試情境）。
func NewNode(store persistence.Store, resolver discovery.TopologyResolver, myIP string, cfg Config) *Node {
	mailboxLimit := cfg.MailboxLimit
	if mailboxLimit == 0 {
		mailboxLimit = defaultMailboxLimit
	}
	mailboxPendingFn := cfg.mailboxPendingFn
	if mailboxPendingFn == nil {
		mailboxPendingFn = actor.ProcessMailboxPending
	}

	n := &Node{
		store:          store,
		resolver:       resolver,
		myIP:           myIP,
		resultCh:       make(chan queuedResult, 10000),
		mailboxLimit:   mailboxLimit,
		mailboxPending: mailboxPendingFn,
		done:           make(chan struct{}),
	}
	for i := 0; i < 256; i++ {
		n.shards[i].actors = make(map[ActorKey]*BusinessActor)
	}

	n.wg.Add(2)
	go n.aggregatorLoop()
	go n.passivationLoop()
	return n
}

// Stop 優雅關閉 Node，嚴格遵守「先殺生產者，再殺消費者」的 Channel 黃金法則。
//
// 關機三段式：
//  1. 停止所有 Actor（生產者）：Actor.Stop() 讓 Actor 執行完手上最後一筆 Handle，
//     結果寫入 resultCh 後才退出。此階段 aggregatorLoop 必須保持活著，
//     否則 resultCh 塞滿後 Actor goroutine 會永遠卡在 Emit，導致 Deadlock。
//     Stop() 是非阻塞的（只設 statusStopping），必須 spin 等到 IsDead() 才能確認
//     Actor goroutine 已完整退出，resultCh 不再有新寫入。
//  2. 關閉 done channel：所有 Actor 已死透，resultCh 不會再有新資料寫入，
//     此時才安全通知 aggregatorLoop 和 passivationLoop 退出。
//  3. Wait：等 aggregatorLoop flush 最後一批結果給 Gateway，確保「遺言」不丟失。
func (n *Node) Stop() {
	// 第一步：發出停止信號並收集所有 Actor 引用，保留引用供後續等待 IsDead()。
	// Stop() 是非阻塞的；必須等 IsDead() 才能確認 goroutine 已完全退出。
	var stopping []*BusinessActor
	for i := 0; i < 256; i++ {
		shard := &n.shards[i]
		shard.Lock()
		for key, bActor := range shard.actors {
			bActor.core.Stop()
			stopping = append(stopping, bActor)
			delete(shard.actors, key)
		}
		shard.Unlock()
	}

	// 等所有 Actor goroutine 真正死透，保證 resultCh 不再有新寫入
	for _, bActor := range stopping {
		for !bActor.core.IsDead() {
			runtime.Gosched()
		}
	}

	// 第二步：Actor 全死，resultCh 不再有新寫入，現在才關消費者
	close(n.done)

	// 第三步：等 aggregatorLoop flush 最後一批結果後自然退出
	n.wg.Wait()
}

// HandleBatch 接收一批 Envelope，通過所有權檢查後派發給對應 Actor。
func (n *Node) HandleBatch(ctx context.Context, batch *pb.BatchRequest, sender remote.BatchSender) error {
	if n.mailboxPending() >= n.mailboxLimit {
		for _, env := range batch.Envelopes {
			if env == nil {
				continue
			}
			n.enqueueResult(sender, &pb.RemoteResult{
				TenantId:  env.TenantId,
				Uid:       env.Uid,
				RequestId: env.RequestId,
				TxId:      env.TxId,
				Success:   false,
				OpCode:    env.OpCode,
				ErrorCode: remote.ErrNodeOverloaded,
				ErrorMsg:  "node mailbox backlog limit reached, please retry",
			})
		}
		return nil
	}

	for _, env := range batch.Envelopes {
		if env.TenantId == 0 || env.Uid == 0 || env.TxId == "" || env.RequestId == 0 {
			n.enqueueResult(sender, &pb.RemoteResult{
				TenantId:  env.TenantId,
				Uid:       env.Uid,
				RequestId: env.RequestId,
				TxId:      env.TxId,
				Success:   false,
				OpCode:    env.OpCode,
				ErrorCode: remote.ErrBadRequest,
				ErrorMsg:  "missing tenant_id, uid, tx_id, or request_id",
			})
			continue
		}

		// ── 所有權防護（O(1)，atomic Load，無鎖）──────────────────────────
		if n.resolver != nil {
			ownerIP, err := n.resolver.GetNodeIP(env.TenantId, env.Uid)
			if err != nil || ownerIP != n.myIP {
				n.forceKillLocalActor(env.TenantId, env.Uid)
				n.enqueueResult(sender, &pb.RemoteResult{
					TenantId:  env.TenantId,
					Uid:       env.Uid,
					RequestId: env.RequestId,
					TxId:      env.TxId,
					Success:   false,
					OpCode:    env.OpCode,
					ErrorCode: remote.ErrWrongNode,
					ErrorMsg:  "routing table indicates this node is not the owner",
				})
				continue
			}
		}

		key := ActorKey{TenantID: env.TenantId, UID: env.Uid}
		shard := &n.shards[key.shardIndex()]

	retry:
		// 1. RLock：快取命中路徑（熱路徑）
		shard.RLock()
		bActor, exists := shard.actors[key]
		shard.RUnlock()

		// 2. Lock：Actor 不存在時建立並 rehydrate
		if !exists {
			shard.Lock()
			bActor, exists = shard.actors[key]
			if !exists {
				bActor = newBusinessActor(key.TenantID, key.UID, n.store, n)
				// 阻塞 I/O：從 Cassandra 重建快照 + Delta Events
				if err := bActor.rehydrate(ctx); err != nil {
					shard.Unlock()
					n.enqueueResult(sender, &pb.RemoteResult{
						TenantId:  env.TenantId,
						Uid:       env.Uid,
						RequestId: env.RequestId,
						TxId:      env.TxId,
						Success:   false,
						ErrorCode: remote.ErrRehydrationFailed,
						ErrorMsg:  err.Error(),
					})
					continue
				}
				shard.actors[key] = bActor
				bActor.core.Start(context.Background())
			}
			shard.Unlock()
		}

		// 3. 無鎖派發到 Actor mailbox（直接傳入 *pb.RemoteEnvelope，無需中間轉換）
		// 多 gRPC stream 併發時，於 Send 前註冊 (tenant,uid,request_id)->sender，由 Emit 取回正確的 BatchSender。
		rk := resultRouteKey{tenant: env.TenantId, uid: env.Uid, req: env.RequestId}
		n.replyRoutes.Store(rk, sender)
		// Send 返回 false 代表 Actor 已被 passivationLoop 停止，出現了停止/發送競態。
		// 清除 Map 中的死亡引用，然後 goto retry 重新建立並派發。
		if !bActor.core.Send(env) {
			n.replyRoutes.Delete(rk)
			shard.Lock()
			if current, ok := shard.actors[key]; ok && current == bActor {
				delete(shard.actors, key)
			}
			shard.Unlock()
			goto retry
		}
	}
	return nil
}

// forceKillLocalActor 從 Shard Map 移除指定 Actor，停止其 goroutine，
// 並在背景將信箱殘留訊息快速回報 WRONG_NODE 錯誤給 Gateway。
//
// 三步驟設計：
//  1. Lock 範圍最小化：只在 Lock 內做 map delete，Stop() 在 Lock 外呼叫。
//     若把 Stop() 放在 Lock 內，Actor 執行最後一筆 Handle（包含 Cassandra write）
//     的期間整個 Shard 都被鎖死，嚴重影響同 Shard 其他 Actor 的吞吐。
//  2. Stop() 是非阻塞的：它只設 statusStopping 並喚醒 loop，goroutine 仍在執行。
//     必須等到 IsDead()（statusDead）才能安全讀取信箱鏈結串列。
//  3. 背景 Drain：等 Actor 完全死亡後，把信箱剩餘訊息抽出並 Emit WRONG_NODE，
//     讓 Gateway 毫秒內收到明確錯誤而非等 Timeout，即時 Retry 到正確節點。
func (n *Node) forceKillLocalActor(tenantID int32, uid int64) {
	key := ActorKey{TenantID: tenantID, UID: uid}
	shard := &n.shards[key.shardIndex()]

	// 步驟 1：Lock 內只做 map delete，最小化鎖持有時間
	shard.Lock()
	bActor, exists := shard.actors[key]
	if exists {
		delete(shard.actors, key)
	}
	shard.Unlock()

	if !exists {
		return
	}

	// 步驟 2：Lock 外 Stop，不阻塞 Shard
	bActor.core.Stop()

	// 步驟 3：背景等 Actor 完全死亡後排空信箱，快速回報錯誤
	// Spin with Gosched：此為低頻路徑（僅在 ownership 變更時觸發），
	// 等待時間上限為 Actor 手上最後一筆 Handle 的執行時間（約一次 Cassandra write）。
	go func() {
		for !bActor.core.IsDead() {
			runtime.Gosched()
		}
		for _, drained := range bActor.core.Drain() {
			rk := resultRouteKey{tenant: drained.TenantId, uid: drained.Uid, req: drained.RequestId}
			if v, ok := n.replyRoutes.LoadAndDelete(rk); ok {
				if reply, ok2 := v.(remote.BatchSender); ok2 {
					n.enqueueResult(reply, &pb.RemoteResult{
						TenantId:  drained.TenantId,
						Uid:       drained.Uid,
						RequestId: drained.RequestId,
						TxId:      drained.TxId,
						Success:   false,
						OpCode:    drained.OpCode,
						ErrorCode: remote.ErrWrongNode,
						ErrorMsg:  "actor evicted due to ownership change, please retry",
					})
				}
			}
		}
	}()
}

// Emit 實作 actor.ResultSink，由 Actor goroutine 呼叫，將結果推入 resultCh。
func (n *Node) Emit(result *pb.RemoteResult) {
	if result == nil {
		return
	}
	rk := resultRouteKey{tenant: result.TenantId, uid: result.Uid, req: result.RequestId}
	v, ok := n.replyRoutes.LoadAndDelete(rk)
	if !ok {
		// 僅在 (tenant,uid,request_id) 未先於 Send 註冊或重複刪除時觸發；不應於正常路徑發生。
		return
	}
	reply, ok := v.(remote.BatchSender)
	if !ok {
		return
	}
	n.resultCh <- queuedResult{res: result, reply: reply}
}

func (n *Node) enqueueResult(reply remote.BatchSender, res *pb.RemoteResult) {
	if reply == nil || res == nil {
		return
	}
	n.resultCh <- queuedResult{res: res, reply: reply}
}

// passivationLoop 每小時掃描全部 Shard，驅逐閒置超過 24 小時的 Actor。
//
// 效能說明：對每個 Shard 持有單次 Lock 而非 RCU 兩階段掃描。
// 在典型的 Shard 大小（幾千個 Actor）下，Lock + 線性掃描的成本遠低於
// 兩次迭代 + slice 分配帶來的 GC 壓力。
func (n *Node) passivationLoop() {
	defer n.wg.Done()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	const idleThreshold = int64(24 * time.Hour)

	for {
		select {
		case <-n.done:
			return
		case <-ticker.C:
			now := time.Now().UnixNano()

			for i := 0; i < 256; i++ {
				shard := &n.shards[i]

				shard.Lock()
				for key, bActor := range shard.actors {
					if now-bActor.lastActive.Load() > idleThreshold {
						bActor.core.Stop()
						delete(shard.actors, key)
					}
				}
				shard.Unlock()

				// 短暫讓出 CPU，避免 256 Shard 連續 Lock 造成 gRPC goroutine 飢餓。
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
}

// aggregatorLoop 把 resultCh 的結果聚合成 batch 後，依筆之 reply stream 分組回傳給各 Gateway / Client。
func (n *Node) aggregatorLoop() {
	defer n.wg.Done()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	var batch []queuedResult
	const maxBatchSize = 1000

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// 依 gRPC stream（BatchSender 實體）分組，避免多 client 時把 A 的結果併到 B 的 BatchResponse。
		groups := make(map[remote.BatchSender][]*pb.RemoteResult, 8)
		for _, q := range batch {
			if q.reply == nil || q.res == nil {
				continue
			}
			groups[q.reply] = append(groups[q.reply], q.res)
		}
		batch = batch[:0]
		for reply, ress := range groups {
			if len(ress) == 0 {
				continue
			}
			for start := 0; start < len(ress); start += maxBatchSize {
				end := start + maxBatchSize
				if end > len(ress) {
					end = len(ress)
				}
				_ = reply.SendBatchResponse(&pb.BatchResponse{Results: ress[start:end]})
			}
		}
	}

	for {
		select {
		case <-n.done:
			flush()
			return
		case pbRes := <-n.resultCh:
			batch = append(batch, pbRes)
			if len(batch) >= maxBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
