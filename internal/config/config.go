package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/ericethridge/tuber/internal/model"
	"gopkg.in/yaml.v3"
)

const (
	DefaultDoiTBaseURL        = "https://api.doit.com"
	DefaultContentTemplate    = "[{{.CloudProvider}}/{{.ClusterName}}] {{.Namespace}}/{{.Kind}}/{{.Name}} {{.Action}}: {{.Summary}}"
	DefaultRateLimitRPS       = 10.0
	DefaultRateLimitBurst     = 20
	DefaultHTTPTimeout        = 15 * time.Second
	DefaultMaxRetries         = 3
	DefaultDedupTTL           = 5 * time.Minute
	DefaultWorkerCount        = 2
	DefaultShutdownTimeout    = 15 * time.Second
)

// Config represents the top-level declarative configuration for tuber.
type Config struct {
	Cluster         ClusterConfig `yaml:"cluster"`
	WatchNamespaces []string      `yaml:"watchNamespaces"`
	DoiT            DoiTConfig    `yaml:"doit"`
	Rollup          RollupConfig  `yaml:"rollup"`
	Rules           []WatchRule   `yaml:"rules"`
	DryRun          bool          `yaml:"dryRun"`

	validateOnly bool `yaml:"-"`
}

// ClusterConfig identifies the Kubernetes cluster and cloud environment.
type ClusterConfig struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"` // e.g., "gcp-gke", "aws-eks"
	Region   string `yaml:"region"`
}

// DoiTConfig configures authentication, targeting, and rate-limiting for the DoiT Annotations API.
// Per https://developer.doit.com/docs/start, shared cluster automation should authenticate using a
// DoiT Service Account API token (created under DoiT Console -> Users and access -> Service Accounts),
// never a deprecated legacy individual API token.
type DoiTConfig struct {
	BaseURL                 string        `yaml:"baseURL"`
	ServiceAccountToken     string        `yaml:"serviceAccountToken"`
	ServiceAccountTokenFile string        `yaml:"serviceAccountTokenFile"`
	// APIToken and APITokenFile are retained as backward-compatible aliases for ServiceAccountToken / ServiceAccountTokenFile.
	APIToken          string        `yaml:"apiToken,omitempty"`
	APITokenFile      string        `yaml:"apiTokenFile,omitempty"`
	TenantID          string        `yaml:"tenantId"`
	ValidateOnStartup *bool         `yaml:"validateOnStartup,omitempty"`
	RejectUserTokens  *bool         `yaml:"rejectUserTokens,omitempty"`
	DefaultReports    []string      `yaml:"defaultReports"`
	DefaultLabels     []string      `yaml:"defaultLabels"`
	ContentTemplate   string        `yaml:"contentTemplate"`
	RateLimitRPS      float64       `yaml:"rateLimitRPS"`
	RateLimitBurst    int           `yaml:"rateLimitBurst"`
	Timeout           time.Duration `yaml:"timeout"`
	MaxRetries        int           `yaml:"maxRetries"`
}

// ValidateOnStartupEnabled returns whether tuber should call GET /auth/v1/validate at startup.
func (d DoiTConfig) ValidateOnStartupEnabled() bool {
	if d.ValidateOnStartup == nil {
		return false
	}
	return *d.ValidateOnStartup
}

// RejectUserTokensEnabled returns whether GET /auth/v1/validate should reject tokens that resolve
// to an individual user email (legacy or personal API tokens) rather than a DoiT Service Account.
func (d DoiTConfig) RejectUserTokensEnabled() bool {
	if d.RejectUserTokens == nil {
		return true
	}
	return *d.RejectUserTokens
}

// RollupConfig controls workload noise suppression, deduplication, and worker concurrency.
type RollupConfig struct {
	IgnoreOwnedPods *bool         `yaml:"ignoreOwnedPods"`
	TrackOOMKilled  *bool         `yaml:"trackOOMKilled"`
	DedupTTL        time.Duration `yaml:"dedupTTL"`
	ResyncPeriod    time.Duration `yaml:"resyncPeriod"`
	Workers         int           `yaml:"workers"`
}

// IgnoreOwnedPodsEnabled returns whether ReplicaSet/Job-owned Pod lifecycle churn is suppressed.
func (r RollupConfig) IgnoreOwnedPodsEnabled() bool {
	if r.IgnoreOwnedPods == nil {
		return true
	}
	return *r.IgnoreOwnedPods
}

// TrackOOMKilledEnabled returns whether container OOMKilled transitions are tracked on watched Pods.
func (r RollupConfig) TrackOOMKilledEnabled() bool {
	if r.TrackOOMKilled == nil {
		return true
	}
	return *r.TrackOOMKilled
}

// WatchRule defines a declarative trigger rule for a Kubernetes resource kind.
// In Phase 1 this backs Deployment, Job, and Pod watchers; in Phase 2 the same schema
// drives dynamic informers for arbitrary GroupVersionResource targets.
type WatchRule struct {
	Name          string        `yaml:"name"`
	APIVersion    string        `yaml:"apiVersion"`
	Kind          string        `yaml:"kind"`
	Namespaces    []string      `yaml:"namespaces,omitempty"`
	LabelSelector string        `yaml:"labelSelector,omitempty"`
	Actions       []model.Action `yaml:"actions"`
	Reports       []string      `yaml:"reports,omitempty"`
	Labels        []string      `yaml:"labels,omitempty"`
}

// AllowsAction checks if the given action is enabled by this rule.
func (r WatchRule) AllowsAction(action model.Action) bool {
	if len(r.Actions) == 0 {
		return true
	}
	for _, a := range r.Actions {
		if strings.EqualFold(string(a), string(action)) {
			return true
		}
	}
	return false
}

// AllowsNamespace checks if the given namespace is allowed by this rule's optional subset filter.
func (r WatchRule) AllowsNamespace(ns string) bool {
	if len(r.Namespaces) == 0 {
		return true
	}
	for _, allowed := range r.Namespaces {
		if allowed == ns {
			return true
		}
	}
	return false
}

// DefaultRules returns the standard Phase 1 workload lifecycle rules.
func DefaultRules() []WatchRule {
	return []WatchRule{
		{
			Name:       "deployments-lifecycle",
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Actions: []model.Action{
				model.ActionCreated,
				model.ActionScaled,
				model.ActionRolledOut,
				model.ActionDeleted,
			},
		},
		{
			Name:       "jobs-lifecycle",
			APIVersion: "batch/v1",
			Kind:       "Job",
			Actions: []model.Action{
				model.ActionCreated,
				model.ActionCompleted,
				model.ActionFailed,
			},
		},
		{
			Name:       "pods-standalone-and-oom",
			APIVersion: "v1",
			Kind:       "Pod",
			Actions: []model.Action{
				model.ActionCreated,
				model.ActionDeleted,
				model.ActionOOMKilled,
			},
		},
	}
}

// Load reads a YAML configuration file (or a Kubernetes ConfigMap YAML manifest containing
// data["config.yaml"]), applies environment variable overrides and defaults, loads secret files
// if configured, and validates the resulting configuration.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}
	return Parse(raw)
}

// LoadValidation reads and validates a configuration file (or Kubernetes ConfigMap manifest)
// for CLI --validate-only checks without requiring in-cluster secret volume files to exist on the local host.
func LoadValidation(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}
	return parseInternal(raw, true)
}

// Parse unmarshals YAML bytes (unwrapping a Kubernetes ConfigMap if present),
// applies environment overrides, defaults, and validates the config.
func Parse(raw []byte) (*Config, error) {
	return parseInternal(raw, false)
}

type k8sConfigMapEnvelope struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   map[string]any    `yaml:"metadata"`
	Data       map[string]string `yaml:"data"`
}

func unwrapConfigMapIfPresent(raw []byte) ([]byte, error) {
	var env k8sConfigMapEnvelope
	if err := yaml.Unmarshal(raw, &env); err == nil {
		if strings.EqualFold(strings.TrimSpace(env.Kind), "ConfigMap") && strings.TrimSpace(env.APIVersion) != "" {
			if inner, ok := env.Data["config.yaml"]; ok && strings.TrimSpace(inner) != "" {
				return []byte(inner), nil
			}
			if len(env.Data) == 1 {
				for _, v := range env.Data {
					if strings.TrimSpace(v) != "" {
						return []byte(v), nil
					}
				}
			}
			return nil, errors.New("Kubernetes ConfigMap does not contain a non-empty 'config.yaml' entry under data")
		}
	}
	return raw, nil
}

func parseInternal(raw []byte, validateOnly bool) (*Config, error) {
	unwrapped, err := unwrapConfigMapIfPresent(raw)
	if err != nil {
		return nil, err
	}

	var cfg Config
	cfg.validateOnly = validateOnly
	dec := yaml.NewDecoder(bytes.NewReader(unwrapped))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decoding config yaml: %w", err)
	}

	applyEnvOverrides(&cfg)
	if err := loadTokenFromFile(&cfg); err != nil {
		return nil, err
	}
	applyDefaults(&cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("TUBER_CLUSTER_NAME")); v != "" {
		cfg.Cluster.Name = v
	}
	if v := strings.TrimSpace(os.Getenv("TUBER_CLOUD_PROVIDER")); v != "" {
		cfg.Cluster.Provider = v
	}
	if v := strings.TrimSpace(os.Getenv("TUBER_CLUSTER_REGION")); v != "" {
		cfg.Cluster.Region = v
	}
	if v := strings.TrimSpace(os.Getenv("TUBER_WATCH_NAMESPACES")); v != "" {
		parts := strings.Split(v, ",")
		var nsList []string
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				nsList = append(nsList, trimmed)
			}
		}
		cfg.WatchNamespaces = nsList
	}
	if v := strings.TrimSpace(os.Getenv("DOIT_BASE_URL")); v != "" {
		cfg.DoiT.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("DOIT_API_TOKEN")); v != "" {
		cfg.DoiT.APIToken = v
	}
	if v := strings.TrimSpace(os.Getenv("DOIT_SERVICE_ACCOUNT_TOKEN")); v != "" {
		cfg.DoiT.ServiceAccountToken = v
	}
	if v := strings.TrimSpace(os.Getenv("DOIT_API_TOKEN_FILE")); v != "" {
		cfg.DoiT.APITokenFile = v
	}
	if v := strings.TrimSpace(os.Getenv("DOIT_SERVICE_ACCOUNT_TOKEN_FILE")); v != "" {
		cfg.DoiT.ServiceAccountTokenFile = v
	}
	if v := strings.TrimSpace(os.Getenv("DOIT_TENANT_ID")); v != "" {
		cfg.DoiT.TenantID = v
	}
	if v := strings.TrimSpace(os.Getenv("DOIT_VALIDATE_ON_STARTUP")); v != "" {
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			b := true
			cfg.DoiT.ValidateOnStartup = &b
		case "false", "0", "no":
			b := false
			cfg.DoiT.ValidateOnStartup = &b
		}
	}
	if v := strings.TrimSpace(os.Getenv("TUBER_DRY_RUN")); v != "" {
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			cfg.DryRun = true
		case "false", "0", "no":
			cfg.DryRun = false
		}
	}
}

func loadTokenFromFile(cfg *Config) error {
	// Normalize ServiceAccountToken <-> APIToken aliases.
	if strings.TrimSpace(cfg.DoiT.ServiceAccountToken) == "" && strings.TrimSpace(cfg.DoiT.APIToken) != "" {
		cfg.DoiT.ServiceAccountToken = cfg.DoiT.APIToken
	}
	if strings.TrimSpace(cfg.DoiT.ServiceAccountTokenFile) == "" && strings.TrimSpace(cfg.DoiT.APITokenFile) != "" {
		cfg.DoiT.ServiceAccountTokenFile = cfg.DoiT.APITokenFile
	}

	if strings.TrimSpace(cfg.DoiT.ServiceAccountToken) == "" && strings.TrimSpace(cfg.DoiT.ServiceAccountTokenFile) != "" {
		data, err := os.ReadFile(cfg.DoiT.ServiceAccountTokenFile)
		if err != nil {
			if (cfg.DryRun || cfg.validateOnly) && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("reading DoiT Service Account token file %q: %w", cfg.DoiT.ServiceAccountTokenFile, err)
		}
		cfg.DoiT.ServiceAccountToken = string(data)
	}

	normalized := normalizeBearerToken(cfg.DoiT.ServiceAccountToken)
	cfg.DoiT.ServiceAccountToken = normalized
	cfg.DoiT.APIToken = normalized
	return nil
}

func normalizeBearerToken(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) >= 7 && strings.EqualFold(trimmed[:7], "bearer ") {
		trimmed = strings.TrimSpace(trimmed[7:])
	}
	return trimmed
}

func applyDefaults(cfg *Config) {
	if strings.TrimSpace(cfg.DoiT.BaseURL) == "" {
		cfg.DoiT.BaseURL = DefaultDoiTBaseURL
	}
	cfg.DoiT.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.DoiT.BaseURL), "/")

	if strings.TrimSpace(cfg.DoiT.ContentTemplate) == "" {
		cfg.DoiT.ContentTemplate = DefaultContentTemplate
	}
	if cfg.DoiT.RateLimitRPS <= 0 {
		cfg.DoiT.RateLimitRPS = DefaultRateLimitRPS
	}
	if cfg.DoiT.RateLimitBurst <= 0 {
		cfg.DoiT.RateLimitBurst = DefaultRateLimitBurst
	}
	if cfg.DoiT.Timeout <= 0 {
		cfg.DoiT.Timeout = DefaultHTTPTimeout
	}
	if cfg.DoiT.MaxRetries < 0 {
		cfg.DoiT.MaxRetries = 0
	} else if cfg.DoiT.MaxRetries == 0 {
		cfg.DoiT.MaxRetries = DefaultMaxRetries
	}
	if cfg.DoiT.RejectUserTokens == nil {
		defaultTrue := true
		cfg.DoiT.RejectUserTokens = &defaultTrue
	}

	if cfg.Rollup.IgnoreOwnedPods == nil {
		defaultTrue := true
		cfg.Rollup.IgnoreOwnedPods = &defaultTrue
	}
	if cfg.Rollup.TrackOOMKilled == nil {
		defaultTrue := true
		cfg.Rollup.TrackOOMKilled = &defaultTrue
	}
	if cfg.Rollup.DedupTTL <= 0 {
		cfg.Rollup.DedupTTL = DefaultDedupTTL
	}
	if cfg.Rollup.Workers <= 0 {
		cfg.Rollup.Workers = DefaultWorkerCount
	}

	if len(cfg.Rules) == 0 {
		cfg.Rules = DefaultRules()
	}
}

// Validate checks that all required fields and invariants are satisfied.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Cluster.Name) == "" {
		return errors.New("cluster.name is required")
	}
	if strings.TrimSpace(c.Cluster.Provider) == "" {
		return errors.New("cluster.provider is required (e.g. 'gcp-gke' or 'aws-eks')")
	}

	if len(c.WatchNamespaces) == 0 {
		return errors.New("watchNamespaces must contain at least one explicit namespace")
	}

	allowedNS := make(map[string]struct{}, len(c.WatchNamespaces))
	dedupNS := make([]string, 0, len(c.WatchNamespaces))
	for _, ns := range c.WatchNamespaces {
		trimmed := strings.TrimSpace(ns)
		if trimmed == "" {
			return errors.New("watchNamespaces cannot contain empty namespace entries")
		}
		if trimmed == "*" || trimmed == "all" {
			return fmt.Errorf("wildcard namespace %q is not allowed; specify an explicit namespace allowlist", trimmed)
		}
		if _, exists := allowedNS[trimmed]; !exists {
			allowedNS[trimmed] = struct{}{}
			dedupNS = append(dedupNS, trimmed)
		}
	}
	c.WatchNamespaces = dedupNS

	if err := validateBaseURL(c.DoiT.BaseURL); err != nil {
		return err
	}

	hasToken := strings.TrimSpace(c.DoiT.ServiceAccountToken) != ""
	hasTokenFileRef := c.validateOnly && strings.TrimSpace(c.DoiT.ServiceAccountTokenFile) != ""
	if !c.DryRun && !hasToken && !hasTokenFileRef {
		return errors.New("doit.serviceAccountToken (or DOIT_SERVICE_ACCOUNT_TOKEN / doit.serviceAccountTokenFile / doit.apiToken) is required when dryRun is false")
	}

	tmpl, err := template.New("annotation").Option("missingkey=error").Parse(c.DoiT.ContentTemplate)
	if err != nil {
		return fmt.Errorf("invalid doit.contentTemplate: %w", err)
	}
	sample := model.WorkloadEvent{
		ClusterName:   c.Cluster.Name,
		CloudProvider: c.Cluster.Provider,
		Region:        c.Cluster.Region,
		Namespace:     c.WatchNamespaces[0],
		Kind:          "Deployment",
		APIVersion:    "apps/v1",
		Name:          "sample-app",
		Action:        model.ActionCreated,
		Summary:       "replicas=3 image=nginx:latest",
		Timestamp:     time.Now().UTC(),
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, sample); err != nil {
		return fmt.Errorf("executing doit.contentTemplate against sample event: %w", err)
	}

	for i, rule := range c.Rules {
		if strings.TrimSpace(rule.Kind) == "" {
			return fmt.Errorf("rules[%d].kind is required", i)
		}
		if strings.TrimSpace(rule.APIVersion) == "" {
			return fmt.Errorf("rules[%d].apiVersion is required", i)
		}
		for _, rns := range rule.Namespaces {
			if _, ok := allowedNS[rns]; !ok {
				return fmt.Errorf("rules[%d] (%s) references namespace %q which is not in watchNamespaces", i, rule.Name, rns)
			}
		}
	}

	return nil
}

func validateBaseURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("doit.baseURL %q must be a valid URL", rawURL)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "https" {
		return nil
	}
	if scheme == "http" && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("doit.baseURL %q must use https:// to protect Bearer tokens in transit (http:// is only permitted for localhost/loopback test servers)", rawURL)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// MatchRule finds the first configured WatchRule matching the given apiVersion, kind, namespace, and action.
func (c *Config) MatchRule(apiVersion, kind, namespace string, action model.Action) (*WatchRule, bool) {
	for i := range c.Rules {
		rule := &c.Rules[i]
		if !strings.EqualFold(rule.APIVersion, apiVersion) || !strings.EqualFold(rule.Kind, kind) {
			continue
		}
		if !rule.AllowsNamespace(namespace) {
			continue
		}
		if !rule.AllowsAction(action) {
			continue
		}
		return rule, true
	}
	return nil, false
}
