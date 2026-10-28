package watcher

import (
	"sync/atomic"
	"testing"

	"github.com/ericethridge/tuber/internal/cluster"
	"github.com/ericethridge/tuber/internal/config"
	"github.com/ericethridge/tuber/internal/doit"
	"github.com/ericethridge/tuber/internal/formatter"
	"github.com/ericethridge/tuber/internal/model"
	"k8s.io/client-go/kubernetes/fake"
)

func TestManagerStampsClusterInfoOnEvents(t *testing.T) {
	cfg, err := config.Parse([]byte(`
cluster:
  name: edge-1
  provider: self-hosted
watchNamespaces: [default]
dryRun: true
`))
	if err != nil {
		t.Fatal(err)
	}
	fmttr, err := formatter.New(cfg.DoiT)
	if err != nil {
		t.Fatal(err)
	}
	sink := doit.NewClient(cfg.DoiT, true)

	var current atomic.Pointer[cluster.Info]
	current.Store(&cluster.Info{Version: "v1.30.2+k3s1", Distribution: "k3s", ID: "uid-1"})
	provider := func() cluster.Info { return *current.Load() }

	tests := []struct {
		name string
		opts []ManagerOption
		want []cluster.Info
	}{
		{name: "no provider leaves fields empty", want: []cluster.Info{{}, {}}},
		{name: "nil provider ignored", opts: []ManagerOption{WithClusterInfo(nil)}, want: []cluster.Info{{}, {}}},
		{
			name: "provider value read per event batch",
			opts: []ManagerOption{WithClusterInfo(provider)},
			want: []cluster.Info{
				{Version: "v1.30.2+k3s1", Distribution: "k3s", ID: "uid-1"},
				{Version: "v1.31.4+k3s1", Distribution: "k3s", ID: "uid-1"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			current.Store(&cluster.Info{Version: "v1.30.2+k3s1", Distribution: "k3s", ID: "uid-1"})
			mgr, err := NewManager(cfg, fake.NewSimpleClientset(), sink, fmttr, nil, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range tc.want {
				ev := model.WorkloadEvent{Namespace: "default", Kind: "Deployment", Name: "app", UID: "u", Action: model.ActionScaled, Generation: int64(i)}
				mgr.enqueueAll([]model.WorkloadEvent{ev})
				got, ok := mgr.items[ev.Fingerprint()]
				if !ok {
					t.Fatalf("event %d was not enqueued", i)
				}
				if got.ClusterVersion != want.Version || got.Distribution != want.Distribution || got.ClusterID != want.ID {
					t.Errorf("event %d stamped %q/%q/%q, want %+v", i, got.ClusterVersion, got.Distribution, got.ClusterID, want)
				}
				current.Store(&cluster.Info{Version: "v1.31.4+k3s1", Distribution: "k3s", ID: "uid-1"})
			}
			mgr.queue.ShutDown()
		})
	}
}

func TestManagerReadsClusterInfoOncePerBatch(t *testing.T) {
	cfg, err := config.Parse([]byte(`
cluster:
  name: edge-1
  provider: self-hosted
watchNamespaces: [default]
dryRun: true
`))
	if err != nil {
		t.Fatal(err)
	}
	fmttr, err := formatter.New(cfg.DoiT)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := func() cluster.Info {
		n := calls.Add(1)
		return cluster.Info{Version: "v1.31.0", Distribution: "unknown", ID: string(rune('0' + n))}
	}
	mgr, err := NewManager(cfg, fake.NewSimpleClientset(), doit.NewClient(cfg.DoiT, true), fmttr, nil, WithClusterInfo(provider))
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.queue.ShutDown()

	mgr.enqueueAll(nil)
	if n := calls.Load(); n != 0 {
		t.Fatalf("provider called %d times for an empty batch, want 0", n)
	}
	batch := []model.WorkloadEvent{
		{Namespace: "default", Kind: "Deployment", Name: "a", UID: "ua", Action: model.ActionScaled},
		{Namespace: "default", Kind: "Deployment", Name: "b", UID: "ub", Action: model.ActionScaled},
	}
	mgr.enqueueAll(batch)
	if n := calls.Load(); n != 1 {
		t.Fatalf("provider called %d times for one batch, want 1", n)
	}
	for _, ev := range batch {
		got, ok := mgr.items[ev.Fingerprint()]
		if !ok {
			t.Fatalf("event %s was not enqueued", ev.Name)
		}
		if got.ClusterID != "1" {
			t.Errorf("event %s ClusterID = %q, want batch snapshot %q", ev.Name, got.ClusterID, "1")
		}
	}
}
