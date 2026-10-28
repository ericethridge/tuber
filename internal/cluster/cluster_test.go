package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ericethridge/tuber/internal/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// fakeDiscovery serves /version from an httptest server so the detector exercises the real
// discovery REST client (and its context handling) while tests control the response.
type fakeDiscovery struct {
	mu      sync.Mutex
	version string
	err     error
	block   chan struct{}
	// aborted counts blocked requests whose context was canceled by the client.
	aborted atomic.Int32
}

func (f *fakeDiscovery) set(v string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version, f.err = v, err
}

func (f *fakeDiscovery) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	v, err, block := f.version, f.err, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			f.aborted.Add(1)
			return
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(version.Info{GitVersion: v})
}

type fakeClient struct {
	*fake.Clientset
	disco *fakeDiscovery
	dc    discovery.DiscoveryInterface
}

func (c *fakeClient) Discovery() discovery.DiscoveryInterface { return c.dc }

func newFakeClient(t *testing.T, gitVersion string, objects ...runtime.Object) *fakeClient {
	t.Helper()
	fd := &fakeDiscovery{version: gitVersion}
	srv := httptest.NewServer(fd)
	t.Cleanup(srv.Close)
	dc, err := discovery.NewDiscoveryClientForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeClient{Clientset: fake.NewSimpleClientset(objects...), disco: fd, dc: dc}
}

func kubeSystem(uid string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(uid)}}
}

func bufferLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestMatchDistribution(t *testing.T) {
	tests := []struct {
		name       string
		user       []config.DistributionRule
		gitVersion string
		want       string
	}{
		{name: "k3s", gitVersion: "v1.31.4+k3s1", want: "k3s"},
		{name: "rke2", gitVersion: "v1.30.2+rke2r1", want: "rke2"},
		{name: "openshift commit suffix", gitVersion: "v1.29.5+29a0d23", want: "openshift"},
		{name: "openshift literal", gitVersion: "v1.11.0+d4cacc0-openshift", want: "openshift"},
		{name: "openshift heuristic needs 7+ hex", gitVersion: "v1.29.5+29a0d2", want: UnknownDistribution},
		{name: "openshift heuristic rejects non-hex", gitVersion: "v1.29.5+29a0d2z", want: UnknownDistribution},
		{name: "plain upstream version is unknown", gitVersion: "v1.31.0", want: UnknownDistribution},
		{name: "eks", gitVersion: "v1.30.4-eks-a737599", want: "eks"},
		{name: "gke", gitVersion: "v1.30.5-gke.1014001", want: "gke"},
		{name: "no built-in aks rule", gitVersion: "v1.29.2-aks.1", want: UnknownDistribution},
		{name: "no match", gitVersion: "v1.31.0-custom", want: UnknownDistribution},
		{name: "unreadable version", gitVersion: "", want: ""},
		{
			name:       "user rule labels plain version kubeadm",
			user:       []config.DistributionRule{{Match: `^v\d+\.\d+\.\d+$`, Name: "kubeadm"}},
			gitVersion: "v1.31.0",
			want:       "kubeadm",
		},
		{
			name:       "user rule labels aks",
			user:       []config.DistributionRule{{Match: `^v1\.29\.`, Name: "aks"}},
			gitVersion: "v1.29.2",
			want:       "aks",
		},
		{
			name:       "user rule wins over openshift heuristic",
			user:       []config.DistributionRule{{Match: `\+[0-9a-f]+$`, Name: "my-build"}},
			gitVersion: "v1.29.5+29a0d23",
			want:       "my-build",
		},
		{
			name:       "user rule",
			user:       []config.DistributionRule{{Match: `\+my-distro`, Name: "my-distro"}},
			gitVersion: "v1.31.0+my-distro",
			want:       "my-distro",
		},
		{
			name:       "user rule overrides built-in",
			user:       []config.DistributionRule{{Match: `\+k3s`, Name: "custom-k3s"}},
			gitVersion: "v1.31.4+k3s1",
			want:       "custom-k3s",
		},
		{
			name: "first user rule wins",
			user: []config.DistributionRule{
				{Match: `-eks-`, Name: "first"},
				{Match: `eks`, Name: "second"},
			},
			gitVersion: "v1.30.4-eks-a737599",
			want:       "first",
		},
		{
			name:       "non-matching user rule falls through to built-in",
			user:       []config.DistributionRule{{Match: `\+my-distro`, Name: "my-distro"}},
			gitVersion: "v1.30.5-gke.1014001",
			want:       "gke",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rules, err := CompileRules(tc.user)
			if err != nil {
				t.Fatalf("CompileRules() error: %v", err)
			}
			if got := MatchDistribution(rules, tc.gitVersion); got != tc.want {
				t.Errorf("MatchDistribution(%q) = %q, want %q", tc.gitVersion, got, tc.want)
			}
		})
	}
}

func TestCompileRulesRejectsInvalidRegex(t *testing.T) {
	_, err := CompileRules([]config.DistributionRule{{Match: `(unclosed`, Name: "bad"}})
	if err == nil || !strings.Contains(err.Error(), "distributions[0]") {
		t.Fatalf("CompileRules() error = %v, want invalid regex error", err)
	}
	if _, err := NewDetector(newFakeClient(t, "v1.31.0"), config.ClusterConfig{
		Distributions: []config.DistributionRule{{Match: `[`, Name: "bad"}},
	}, nil); err == nil {
		t.Fatal("NewDetector() accepted an invalid regex")
	}
}

func TestDetect(t *testing.T) {
	forbidden := func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "kube-system", errors.New("rbac"))
	}
	tests := []struct {
		name       string
		cfg        config.ClusterConfig
		version    string
		versionErr error
		objects    []runtime.Object
		reactor    k8stesting.ReactionFunc
		want       Info
		wantLog    string
	}{
		{
			name:    "success with detected ID",
			cfg:     config.ClusterConfig{DetectID: true},
			version: "v1.31.4+k3s1",
			objects: []runtime.Object{kubeSystem("uid-123")},
			want:    Info{Version: "v1.31.4+k3s1", Distribution: "k3s", ID: "uid-123"},
		},
		{
			name:    "detectID disabled skips ID lookup",
			cfg:     config.ClusterConfig{},
			version: "v1.31.0",
			objects: []runtime.Object{kubeSystem("uid-123")},
			want:    Info{Version: "v1.31.0", Distribution: UnknownDistribution},
		},
		{
			name:    "cluster.id wins over detection",
			cfg:     config.ClusterConfig{ID: "static-id", DetectID: true},
			version: "v1.31.0",
			objects: []runtime.Object{kubeSystem("uid-123")},
			want:    Info{Version: "v1.31.0", Distribution: UnknownDistribution, ID: "static-id"},
		},
		{
			name:    "forbidden namespace read",
			cfg:     config.ClusterConfig{DetectID: true},
			version: "v1.31.0",
			reactor: forbidden,
			want:    Info{Version: "v1.31.0", Distribution: UnknownDistribution},
			wantLog: "namespaces/kube-system",
		},
		{
			name:    "kube-system not found",
			cfg:     config.ClusterConfig{DetectID: true},
			version: "v1.31.0",
			want:    Info{Version: "v1.31.0", Distribution: UnknownDistribution},
			wantLog: "not found",
		},
		{
			name:       "version failure",
			cfg:        config.ClusterConfig{DetectID: true},
			versionErr: errors.New("connection refused"),
			objects:    []runtime.Object{kubeSystem("uid-123")},
			want:       Info{ID: "uid-123"},
			wantLog:    "connection refused",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeClient(t, tc.version, tc.objects...)
			client.disco.set(tc.version, tc.versionErr)
			if tc.reactor != nil {
				client.PrependReactor("get", "namespaces", tc.reactor)
			}
			logger, buf := bufferLogger()
			d, err := NewDetector(client, tc.cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			got := d.Refresh(context.Background())
			if got != tc.want {
				t.Errorf("Refresh() = %+v, want %+v", got, tc.want)
			}
			if d.Current() != tc.want {
				t.Errorf("Current() = %+v, want %+v", d.Current(), tc.want)
			}
			if tc.wantLog != "" && !strings.Contains(buf.String(), tc.wantLog) {
				t.Errorf("log %q missing %q", buf.String(), tc.wantLog)
			}
		})
	}
}

func TestDetectPackageFunc(t *testing.T) {
	got := Detect(context.Background(), newFakeClient(t, "v1.30.5-gke.1014001"), config.ClusterConfig{ID: "my-id"})
	want := Info{Version: "v1.30.5-gke.1014001", Distribution: "gke", ID: "my-id"}
	if got != want {
		t.Errorf("Detect() = %+v, want %+v", got, want)
	}
}

func TestDetectTimeoutKeepsPreviousValue(t *testing.T) {
	client := newFakeClient(t, "v1.31.0")
	logger, buf := bufferLogger()
	d, err := NewDetector(client, config.ClusterConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	d.timeout = 50 * time.Millisecond
	first := d.Refresh(context.Background())

	release := make(chan struct{})
	defer close(release)
	client.disco.mu.Lock()
	client.disco.block = release
	client.disco.mu.Unlock()

	start := time.Now()
	got := d.Refresh(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Refresh() took %s; timeout not enforced", elapsed)
	}
	if got != first {
		t.Errorf("Refresh() after timeout = %+v, want previous %+v", got, first)
	}
	if !strings.Contains(buf.String(), "deadline exceeded") {
		t.Errorf("expected timeout warning, log: %s", buf.String())
	}
	// The context must cancel the HTTP request itself, not just stop waiting for it.
	deadline := time.Now().Add(2 * time.Second)
	for client.disco.aborted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if client.disco.aborted.Load() == 0 {
		t.Error("timed-out /version request was not canceled on the server side")
	}
}

func TestServerVersionFallsBackWithoutRESTClient(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.30.4-eks-a737599"}
	got := Detect(context.Background(), cs, config.ClusterConfig{})
	if want := (Info{Version: "v1.30.4-eks-a737599", Distribution: "eks"}); got != want {
		t.Errorf("Detect() = %+v, want %+v", got, want)
	}
}

func TestIDFailureWarnsOnceThenDebug(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		message string
	}{
		{
			name:    "forbidden",
			err:     apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "kube-system", errors.New("rbac")),
			message: "cluster ID detection forbidden",
		},
		{
			name:    "not found",
			err:     apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "kube-system"),
			message: "cluster ID detection failed: kube-system namespace not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeClient(t, "v1.31.0")
			fail := true
			client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
				if fail {
					return true, nil, tc.err
				}
				return true, kubeSystem("uid-ok"), nil
			})
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			d, err := NewDetector(client, config.ClusterConfig{DetectID: true}, logger)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			for i := 0; i < 3; i++ {
				d.Refresh(ctx)
			}
			if n := strings.Count(buf.String(), "level=WARN msg=\""+tc.message); n != 1 {
				t.Errorf("warnings = %d, want 1; log:\n%s", n, buf.String())
			}
			if !strings.Contains(buf.String(), "will retry every 1h0m0s") {
				t.Errorf("warning does not mention retry interval; log:\n%s", buf.String())
			}
			if n := strings.Count(buf.String(), "level=DEBUG msg=\""+tc.message); n != 2 {
				t.Errorf("debug repeats = %d, want 2; log:\n%s", n, buf.String())
			}
			fail = false
			if got := d.Refresh(ctx); got.ID != "uid-ok" {
				t.Fatalf("ID after recovery = %q, want uid-ok", got.ID)
			}
			if d.idWarned {
				t.Error("idWarned not reset after a successful lookup")
			}
		})
	}
}

func TestIDFailureWarningWithRechecksDisabled(t *testing.T) {
	client := newFakeClient(t, "v1.31.0")
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "kube-system", errors.New("rbac"))
	})
	logger, buf := bufferLogger()
	zero := config.Duration(0)
	d, err := NewDetector(client, config.ClusterConfig{DetectID: true, DetectInterval: &zero}, logger)
	if err != nil {
		t.Fatal(err)
	}
	d.Refresh(context.Background())
	if !strings.Contains(buf.String(), "rechecks are disabled") {
		t.Errorf("warning does not mention disabled rechecks; log:\n%s", buf.String())
	}
}

func TestRefreshKeepsLastGoodValueAndStickyID(t *testing.T) {
	client := newFakeClient(t, "v1.30.2+k3s1")
	failNamespace := true
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failNamespace {
			return true, nil, errors.New("apiserver unavailable")
		}
		return true, kubeSystem("uid-first"), nil
	})
	logger, buf := bufferLogger()
	d, err := NewDetector(client, config.ClusterConfig{DetectID: true}, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Startup: version read, ID lookup fails.
	if got, want := d.Refresh(ctx), (Info{Version: "v1.30.2+k3s1", Distribution: "k3s"}); got != want {
		t.Fatalf("initial Refresh() = %+v, want %+v", got, want)
	}

	// Recheck: version fails (keep last good), ID retried and now succeeds.
	client.disco.set("", errors.New("timeout"))
	failNamespace = false
	if got, want := d.Refresh(ctx), (Info{Version: "v1.30.2+k3s1", Distribution: "k3s", ID: "uid-first"}); got != want {
		t.Fatalf("second Refresh() = %+v, want %+v", got, want)
	}

	// Upgrade: version changes and is logged; ID is not re-read even if the lookup would differ.
	client.disco.set("v1.31.4+k3s1", nil)
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, kubeSystem("uid-second"), nil
	})
	if got, want := d.Refresh(ctx), (Info{Version: "v1.31.4+k3s1", Distribution: "k3s", ID: "uid-first"}); got != want {
		t.Fatalf("third Refresh() = %+v, want %+v", got, want)
	}
	if !strings.Contains(buf.String(), "cluster version or distribution changed") {
		t.Errorf("version change not logged: %s", buf.String())
	}
	if n := len(client.Actions()); n != 2 {
		t.Errorf("namespace lookups = %d, want 2 (ID must be sticky once read)", n)
	}
}

func TestRunDisabledAndTicker(t *testing.T) {
	zero := config.Duration(0)
	d, err := NewDetector(newFakeClient(t, "v1.31.0"), config.ClusterConfig{DetectInterval: &zero}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { d.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run() with interval 0 did not return")
	}

	client := newFakeClient(t, "v1.31.0")
	d, err = NewDetector(client, config.ClusterConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for d.Current().Version == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if d.Current().Version != "v1.31.0" {
		t.Errorf("ticker did not refresh: %+v", d.Current())
	}
}

func TestConcurrentRefreshAndRead(t *testing.T) {
	client := newFakeClient(t, "v1.30.0", kubeSystem("uid-race"))
	d, err := NewDetector(client, config.ClusterConfig{DetectID: true}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					info := d.Current()
					if info.Version != "" && info.Distribution == "" {
						t.Errorf("torn read: %+v", info)
						return
					}
				}
			}
		}()
	}
	versions := []string{"v1.30.0", "v1.31.0+k3s1", "v1.31.0-eks-abc"}
	for i := 0; i < 200; i++ {
		client.disco.set(versions[i%len(versions)], nil)
		d.Refresh(ctx)
	}
	close(stop)
	wg.Wait()
	if d.Current().ID != "uid-race" {
		t.Errorf("ID = %q, want uid-race", d.Current().ID)
	}
}
