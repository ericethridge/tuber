// Package cluster detects the Kubernetes version, distribution, and a stable cluster ID
// so they can be stamped on workload events. Detection never blocks startup: failures
// log a warning and leave the affected fields empty.
package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ericethridge/tuber/internal/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
)

const (
	// DefaultTimeout bounds a single detection pass (version lookup plus optional ID lookup).
	DefaultTimeout = 5 * time.Second
	// UnknownDistribution is reported when the version is readable but no rule matches.
	UnknownDistribution = "unknown"

	idNamespace = "kube-system"
)

// Info is the detected cluster metadata stamped on every workload event.
type Info struct {
	Version      string
	Distribution string
	ID           string
}

// Rule maps a compiled gitVersion pattern to a distribution name.
type Rule struct {
	Match *regexp.Regexp
	Name  string
}

// builtinRules are evaluated after user rules, in order. Patterns match the API server's
// /version gitVersion only, so no extra permissions are needed. kubeadm, AKS, kind, minikube,
// Talos, and vanilla clusters all report a plain vX.Y.Z and cannot be told apart from the
// version string, so there is no built-in rule for them: a plain version is UnknownDistribution
// and users add cluster.distributions rules for those platforms.
var builtinRules = []Rule{
	{Match: regexp.MustCompile(`\+k3s`), Name: "k3s"},
	{Match: regexp.MustCompile(`\+rke2`), Name: "rke2"},
	// Heuristic: OpenShift 4 reports upstream versions suffixed with a short commit hash,
	// e.g. v1.29.5+29a0d23. Any other build with a +<7+ hex> suffix is also labeled openshift.
	{Match: regexp.MustCompile(`(?i)openshift|^v\d+\.\d+\.\d+\+[0-9a-f]{7,}$`), Name: "openshift"},
	{Match: regexp.MustCompile(`-eks-`), Name: "eks"},
	{Match: regexp.MustCompile(`-gke\.`), Name: "gke"},
}

// CompileRules compiles user distribution rules and appends the built-in rules after them.
func CompileRules(user []config.DistributionRule) ([]Rule, error) {
	rules := make([]Rule, 0, len(user)+len(builtinRules))
	for i, r := range user {
		re, err := regexp.Compile(r.Match)
		if err != nil {
			return nil, fmt.Errorf("cluster.distributions[%d].match %q: %w", i, r.Match, err)
		}
		rules = append(rules, Rule{Match: re, Name: r.Name})
	}
	return append(rules, builtinRules...), nil
}

// MatchDistribution returns the name of the first rule matching gitVersion, UnknownDistribution
// when none match, or an empty string when gitVersion is empty (version unreadable).
func MatchDistribution(rules []Rule, gitVersion string) string {
	if gitVersion == "" {
		return ""
	}
	for _, r := range rules {
		if r.Match.MatchString(gitVersion) {
			return r.Name
		}
	}
	return UnknownDistribution
}

// Detector performs detection passes and holds the current Info for concurrent readers.
type Detector struct {
	client   kubernetes.Interface
	rules    []Rule
	staticID string
	detectID bool
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger

	refreshMu sync.Mutex
	current   atomic.Pointer[Info]
	// idWarned records that a persistent (forbidden or not-found) ID failure was already logged
	// at Warn; repeats log at Debug until a lookup succeeds. Guarded by refreshMu.
	idWarned bool
}

// NewDetector builds a Detector from the cluster configuration.
func NewDetector(client kubernetes.Interface, cfg config.ClusterConfig, logger *slog.Logger) (*Detector, error) {
	if client == nil {
		return nil, errors.New("kubernetes client cannot be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	rules, err := CompileRules(cfg.Distributions)
	if err != nil {
		return nil, err
	}
	d := &Detector{
		client:   client,
		rules:    rules,
		staticID: cfg.ID,
		detectID: cfg.DetectID,
		interval: cfg.DetectIntervalOrDefault(),
		timeout:  DefaultTimeout,
		logger:   logger,
	}
	d.current.Store(&Info{ID: cfg.ID})
	return d, nil
}

// Detect runs a single detection pass and returns the result. Failures are logged as warnings.
func Detect(ctx context.Context, client kubernetes.Interface, cfg config.ClusterConfig) Info {
	d, err := NewDetector(client, cfg, slog.Default())
	if err != nil {
		slog.Default().Warn("cluster detection skipped", "error", err)
		return Info{ID: cfg.ID}
	}
	return d.Refresh(ctx)
}

// Current returns the latest known Info. It is safe for concurrent use.
func (d *Detector) Current() Info {
	return *d.current.Load()
}

// Refresh runs a detection pass and merges it into the current Info: a failed version lookup
// keeps the previous version and distribution, and an ID once read is never replaced.
func (d *Detector) Refresh(ctx context.Context) Info {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()

	prev := d.Current()
	next := prev

	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	if gitVersion, err := d.serverVersion(ctx); err != nil {
		d.logger.Warn("cluster version detection failed; keeping previous value",
			"error", err,
			"version", prev.Version,
			"distribution", prev.Distribution,
		)
	} else {
		next.Version = gitVersion
		next.Distribution = MatchDistribution(d.rules, gitVersion)
	}

	if next.ID == "" && d.staticID == "" && d.detectID {
		if id, err := d.namespaceUID(ctx); err != nil {
			d.logWarnID(err)
		} else {
			next.ID = id
			d.idWarned = false
		}
	}

	if prev.Version != "" && (prev.Version != next.Version || prev.Distribution != next.Distribution) {
		d.logger.Info("cluster version or distribution changed",
			"previousVersion", prev.Version,
			"version", next.Version,
			"previousDistribution", prev.Distribution,
			"distribution", next.Distribution,
		)
	}
	if prev.ID == "" && next.ID != "" {
		d.logger.Info("detected cluster ID from kube-system namespace UID", "clusterId", next.ID)
	}

	d.current.Store(&next)
	return next
}

// Run reruns detection every configured interval until ctx is canceled. It returns
// immediately when the interval is 0 (rechecks disabled).
func (d *Detector) Run(ctx context.Context) {
	if d.interval <= 0 {
		return
	}
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Refresh(ctx)
		}
	}
}

// serverVersion reads /version through the discovery REST client so ctx cancels the request
// itself and no goroutine is left behind on a hung API server. Discovery implementations without
// a REST client (fake clientsets) fall back to the context-free ServerVersion call.
func (d *Detector) serverVersion(ctx context.Context) (string, error) {
	var info *version.Info
	disco := d.client.Discovery()
	if rc := disco.RESTClient(); rc != nil {
		body, err := rc.Get().AbsPath("/version").Do(ctx).Raw()
		if err != nil {
			return "", fmt.Errorf("reading API server version: %w", err)
		}
		info = &version.Info{}
		if err := json.Unmarshal(body, info); err != nil {
			return "", fmt.Errorf("parsing API server version: %w", err)
		}
	} else {
		var err error
		if info, err = disco.ServerVersion(); err != nil {
			return "", fmt.Errorf("reading API server version: %w", err)
		}
	}
	if info == nil || info.GitVersion == "" {
		return "", errors.New("API server reported an empty gitVersion")
	}
	return info.GitVersion, nil
}

func (d *Detector) namespaceUID(ctx context.Context) (string, error) {
	ns, err := d.client.CoreV1().Namespaces().Get(ctx, idNamespace, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if ns.UID == "" {
		return "", errors.New("kube-system namespace has an empty UID")
	}
	return string(ns.UID), nil
}

// logWarnID logs an ID lookup failure. Forbidden and not-found errors persist until RBAC or
// config changes, so they warn once and then log at Debug on each retry. Callers hold refreshMu.
func (d *Detector) logWarnID(err error) {
	var msg, hint string
	switch {
	case apierrors.IsForbidden(err):
		msg = "cluster ID detection forbidden; continuing with empty ID"
		hint = "Grant get on namespaces/kube-system (Helm: cluster.detectID=true with rbac.create=true) or set cluster.id."
	case apierrors.IsNotFound(err):
		msg = "cluster ID detection failed: kube-system namespace not found; continuing with empty ID"
		hint = "Set cluster.id to provide a stable cluster ID."
	default:
		d.logger.Warn("cluster ID detection failed; will retry on next recheck",
			"error", err,
		)
		return
	}
	if d.idWarned {
		d.logger.Debug(msg, "error", err)
		return
	}
	d.idWarned = true
	retry := "rechecks are disabled (cluster.detectInterval=0); restart to retry"
	if d.interval > 0 {
		retry = "will retry every " + d.interval.String() + "; repeats are logged at debug level"
	}
	d.logger.Warn(msg+"; "+retry, "error", err, "hint", hint)
}
