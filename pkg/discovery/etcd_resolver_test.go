package discovery

import (
	"errors"
	"testing"

	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func buildResolverFromNodes(t *testing.T, nodes []string) *EtcdResolver {
	t.Helper()

	r := NewEtcdResolver(nil, "/actor_cluster/nodes")
	r.tab.Store(BuildRoutingTable(nodes, nil))
	return r
}

func TestSortedNodeIPsFromKVs(t *testing.T) {
	t.Parallel()

	kvs := []*mvccpb.KeyValue{
		{Key: []byte("/actor_cluster/nodes/10.0.0.3:9000")},
		{Key: []byte("/actor_cluster/nodes/10.0.0.1:9000")},
		{Key: []byte("/actor_cluster/nodes/10.0.0.2:9000")},
	}

	got := sortedNodeIPsFromKVs("/actor_cluster/nodes/", kvs)
	want := []string{"10.0.0.1:9000", "10.0.0.2:9000", "10.0.0.3:9000"}

	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHasNodeTopologyEvent(t *testing.T) {
	t.Parallel()

	if !hasNodeTopologyEvent([]*clientv3.Event{{Type: clientv3.EventTypePut}}) {
		t.Fatal("expected put event to trigger rebuild")
	}
	if !hasNodeTopologyEvent([]*clientv3.Event{{Type: clientv3.EventTypeDelete}}) {
		t.Fatal("expected delete event to trigger rebuild")
	}
	if hasNodeTopologyEvent(nil) {
		t.Fatal("expected nil events to be ignored")
	}
}

func TestNormalizeNodePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		output string
	}{
		{
			name:   "already normalized",
			input:  "/actor_cluster/nodes/",
			output: "/actor_cluster/nodes/",
		},
		{
			name:   "missing trailing slash",
			input:  "/actor_cluster/nodes",
			output: "/actor_cluster/nodes/",
		},
		{
			name:   "empty prefix becomes root",
			input:  "",
			output: "/",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := normalizeNodePrefix(tc.input); got != tc.output {
				t.Fatalf("normalizeNodePrefix(%q) = %q, want %q", tc.input, got, tc.output)
			}
		})
	}
}

func TestGetNodeIP(t *testing.T) {
	t.Parallel()

	nodes := []string{"10.0.0.1:9000", "10.0.0.2:9000"}
	r := buildResolverFromNodes(t, nodes)

	tests := []struct {
		name   string
		tenant int32
		uid    int64
	}{
		{name: "small uid", tenant: 1, uid: 100},
		{name: "different tenant", tenant: 2, uid: 100},
		{name: "large uid", tenant: 1, uid: 1<<40 + 512},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			slot := SlotOf(tc.tenant, tc.uid)
			wantIP := nodes[slot%len(nodes)]

			gotIP, err := r.GetNodeIP(tc.tenant, tc.uid)
			if err != nil {
				t.Fatalf("GetNodeIP(%d, %d) unexpected err = %v", tc.tenant, tc.uid, err)
			}
			if gotIP != wantIP {
				t.Fatalf("GetNodeIP(%d, %d) = %q, want %q", tc.tenant, tc.uid, gotIP, wantIP)
			}
		})
	}
}

func TestGetNodeIP_UninitializedTable(t *testing.T) {
	t.Parallel()

	r := NewEtcdResolver(nil, "/actor_cluster/nodes")
	_, err := r.GetNodeIP(1, 1)
	if !errors.Is(err, ErrRoutingTableNotInitialized) {
		t.Fatalf("expected ErrRoutingTableNotInitialized, got %v", err)
	}
}

func TestGetNodeIP_EmptyRoutingTable(t *testing.T) {
	t.Parallel()

	r := buildResolverFromNodes(t, nil)
	_, err := r.GetNodeIP(1, 1)
	if !errors.Is(err, ErrSlotUnassigned) {
		t.Fatalf("expected ErrSlotUnassigned, got %v", err)
	}
}

func TestCopyOnWrite_IsolatesOldReaders(t *testing.T) {
	t.Parallel()

	nodes := []string{"stable-node:9000", "new-node:9000"}
	r := buildResolverFromNodes(t, []string{"stable-node:9000"})

	oldTable := r.tab.Load()
	oldIP := oldTable[0]

	r.tab.Store(BuildRoutingTable(nodes, nil))

	if oldTable[0] != oldIP {
		t.Fatalf("CoW violated: old reader snapshot mutated, got %q, want %q", oldTable[0], oldIP)
	}

	slot := SlotOf(1, 0)
	wantIP := nodes[slot%len(nodes)]

	gotIP, err := r.GetNodeIP(1, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotIP != wantIP {
		t.Fatalf("after CoW update, GetNodeIP(1, 0) = %q, want %q", gotIP, wantIP)
	}
}
