package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericethridge/tuber/internal/model"
)

func TestLoadPreviewSkipsCredentialsButValidatesConfiguration(t *testing.T) {
	for _, key := range []string{"DOIT_SERVICE_ACCOUNT_TOKEN", "DOIT_API_TOKEN", "DOIT_SERVICE_ACCOUNT_TOKEN_FILE", "DOIT_API_TOKEN_FILE", "TUBER_DRY_RUN"} {
		t.Setenv(key, "")
	}
	for _, tc := range []struct {
		name    string
		extra   string
		wantErr string
	}{
		{name: "no credentials"},
		{name: "unreadable token file", extra: "doit:\n  serviceAccountTokenFile: " + t.TempDir()},
		{name: "unreadable legacy token file", extra: "doit:\n  apiTokenFile: " + t.TempDir()},
		{name: "unknown field", extra: "watchNamespace: [default]", wantErr: "watchNamespace"},
		{name: "bad template", extra: "doit:\n  contentTemplate: '{{.Nonexistent}}'", wantErr: "contentTemplate"},
		{name: "invalid namespaces", extra: "rules:\n  - apiVersion: apps/v1\n    kind: Deployment\n    namespaces: [unwatched]", wantErr: "not in watchNamespaces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			raw := "cluster:\n  name: test\n  provider: gcp-gke\nwatchNamespaces: [default]\n" + tc.extra + "\n"
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadPreview(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadPreview error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("offline preview required credentials: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("runtime loading must still require readable credentials")
			}
		})
	}
}

func TestParseValidConfigGKEAndEKS(t *testing.T) {
	tests := []struct {
		name         string
		yaml         string
		wantProvider string
		wantNS       []string
	}{
		{
			name: "GKE config with defaults",
			yaml: `
cluster:
  name: prod-gke-us-central1
  provider: gcp-gke
  region: us-central1
watchNamespaces:
  - production
  - payments
doit:
  apiToken: test-token-123
  tenantId: tenant-abc
  defaultReports:
    - report-1
  defaultLabels:
    - label-1
`,
			wantProvider: "gcp-gke",
			wantNS:       []string{"production", "payments"},
		},
		{
			name: "EKS config with custom rules",
			yaml: `
cluster:
  name: prod-eks-us-east-1
  provider: aws-eks
  region: us-east-1
watchNamespaces:
  - default
  - batch-jobs
doit:
  apiToken: eks-token-456
rules:
  - name: custom-deployments
    apiVersion: apps/v1
    kind: Deployment
    namespaces:
      - default
    actions:
      - Created
      - RolledOut
    reports:
      - eks-report-1
`,
			wantProvider: "aws-eks",
			wantNS:       []string{"default", "batch-jobs"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("Parse() unexpected error: %v", err)
			}
			if cfg.Cluster.Provider != tc.wantProvider {
				t.Errorf("Provider = %q, want %q", cfg.Cluster.Provider, tc.wantProvider)
			}
			if len(cfg.WatchNamespaces) != len(tc.wantNS) {
				t.Fatalf("WatchNamespaces = %v, want %v", cfg.WatchNamespaces, tc.wantNS)
			}
			if cfg.DoiT.BaseURL != DefaultDoiTBaseURL {
				t.Errorf("BaseURL = %q, want %q", cfg.DoiT.BaseURL, DefaultDoiTBaseURL)
			}
			if !cfg.Rollup.IgnoreOwnedPodsEnabled() {
				t.Errorf("expected IgnoreOwnedPodsEnabled() to default to true")
			}
			if !cfg.Rollup.TrackOOMKilledEnabled() {
				t.Errorf("expected TrackOOMKilledEnabled() to default to true")
			}
			if len(cfg.Rules) == 0 {
				t.Errorf("expected non-empty rules")
			}
		})
	}
}

func TestParseValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "missing watchNamespaces",
			yaml: `
cluster:
  name: gke-1
  provider: gcp-gke
doit:
  apiToken: token
`,
			wantErr: "watchNamespaces must contain at least one explicit namespace",
		},
		{
			name: "wildcard watchNamespaces rejected",
			yaml: `
cluster:
  name: gke-1
  provider: gcp-gke
watchNamespaces:
  - "*"
doit:
  apiToken: token
`,
			wantErr: "wildcard namespace",
		},
		{
			name: "missing apiToken when dryRun is false",
			yaml: `
cluster:
  name: eks-1
  provider: aws-eks
watchNamespaces:
  - production
`,
			wantErr: "doit.apiToken",
		},
		{
			name: "invalid template field",
			yaml: `
cluster:
  name: eks-1
  provider: aws-eks
watchNamespaces:
  - production
doit:
  apiToken: token
  contentTemplate: "{{.NonExistentField}}"
`,
			wantErr: "executing doit.contentTemplate",
		},
		{
			name: "rule namespace not in watchNamespaces allowlist",
			yaml: `
cluster:
  name: eks-1
  provider: aws-eks
watchNamespaces:
  - production
doit:
  apiToken: token
rules:
  - name: bad-ns
    apiVersion: apps/v1
    kind: Deployment
    namespaces:
      - staging
`,
			wantErr: "not in watchNamespaces",
		},
		{
			name: "cleartext http baseURL to remote host rejected",
			yaml: `
cluster:
  name: eks-1
  provider: aws-eks
watchNamespaces:
  - production
doit:
  baseURL: http://api.doit.com
  serviceAccountToken: token
`,
			wantErr: "must use https://",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadTokenFileAndEnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	tokenFile := filepath.Join(tmpDir, "api-token")
	if err := os.WriteFile(tokenFile, []byte("  secret-file-token-789 \n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}

	cfgPath := filepath.Join(tmpDir, "config.yaml")
	yamlContent := `
cluster:
  name: initial-cluster
  provider: gcp-gke
watchNamespaces:
  - app-ns
doit:
  apiTokenFile: ` + tokenFile + `
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	t.Setenv("TUBER_CLUSTER_NAME", "overridden-eks-cluster")
	t.Setenv("TUBER_CLOUD_PROVIDER", "aws-eks")
	t.Setenv("DOIT_TENANT_ID", "tenant-override-99")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.DoiT.APIToken != "secret-file-token-789" {
		t.Errorf("APIToken = %q, want %q", cfg.DoiT.APIToken, "secret-file-token-789")
	}
	if cfg.Cluster.Name != "overridden-eks-cluster" {
		t.Errorf("Cluster.Name = %q, want %q", cfg.Cluster.Name, "overridden-eks-cluster")
	}
	if cfg.Cluster.Provider != "aws-eks" {
		t.Errorf("Cluster.Provider = %q, want %q", cfg.Cluster.Provider, "aws-eks")
	}
	if cfg.DoiT.TenantID != "tenant-override-99" {
		t.Errorf("DoiT.TenantID = %q, want %q", cfg.DoiT.TenantID, "tenant-override-99")
	}

	rule, ok := cfg.MatchRule("apps/v1", "Deployment", "app-ns", model.ActionScaled)
	if !ok || rule == nil {
		t.Fatalf("expected default rule to match Deployment Scaled in app-ns")
	}
}

func TestDryRunAllowsMissingTokenFile(t *testing.T) {
	cfg, err := Parse([]byte(`
cluster:
  name: dryrun-gke
  provider: gcp-gke
watchNamespaces:
  - default
dryRun: true
doit:
  serviceAccountTokenFile: /nonexistent/path/to/doit/service-account-token
`))
	if err != nil {
		t.Fatalf("expected dryRun=true to ignore missing serviceAccountTokenFile, got error: %v", err)
	}
	if !cfg.DryRun {
		t.Errorf("expected DryRun=true")
	}
}

func TestServiceAccountTokenNormalizationAndEnv(t *testing.T) {
	t.Setenv("DOIT_SERVICE_ACCOUNT_TOKEN", "  Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.sa-token  ")

	cfg, err := Parse([]byte(`
cluster:
  name: prod-gke
  provider: gcp-gke
watchNamespaces:
  - production
doit:
  validateOnStartup: true
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	want := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.sa-token"
	if cfg.DoiT.ServiceAccountToken != want {
		t.Errorf("ServiceAccountToken = %q, want %q", cfg.DoiT.ServiceAccountToken, want)
	}
	if cfg.DoiT.APIToken != want {
		t.Errorf("APIToken alias = %q, want %q", cfg.DoiT.APIToken, want)
	}
	if !cfg.DoiT.ValidateOnStartupEnabled() {
		t.Error("expected ValidateOnStartupEnabled() == true")
	}
	if !cfg.DoiT.RejectUserTokensEnabled() {
		t.Error("expected RejectUserTokensEnabled() to default to true")
	}
}

func TestLoadValidationUnwrapsKubernetesConfigMapManifests(t *testing.T) {
	paths := []string{
		filepath.Join("..", "..", "deploy", "kustomize", "base", "configmap.yaml"),
		filepath.Join("..", "..", "deploy", "kustomize", "overlays", "gke", "patch-config.yaml"),
		filepath.Join("..", "..", "deploy", "kustomize", "overlays", "eks", "patch-config.yaml"),
	}
	for _, p := range paths {
		cfg, err := LoadValidation(p)
		if err != nil {
			t.Fatalf("LoadValidation(%q) unexpected error: %v", p, err)
		}
		if len(cfg.WatchNamespaces) == 0 {
			t.Errorf("LoadValidation(%q) returned empty WatchNamespaces", p)
		}
	}
}

func TestClusterDetectionConfig(t *testing.T) {
	t.Setenv("TUBER_CLUSTER_ID", "")
	base := "cluster:\n  name: edge\n  provider: self-hosted\n"
	tail := "watchNamespaces: [default]\ndryRun: true\n"
	var manyRules strings.Builder
	manyRules.WriteString("  distributions:\n")
	for i := 0; i <= MaxDistributionRules; i++ {
		manyRules.WriteString("    - {match: 'x', name: d}\n")
	}
	tests := []struct {
		name         string
		cluster      string
		wantErr      string
		wantInterval time.Duration
		wantRules    int
	}{
		{name: "defaults", wantInterval: DefaultDetectInterval},
		{name: "zero disables", cluster: "  detectInterval: 0s\n", wantInterval: 0},
		{name: "integer zero disables", cluster: "  detectInterval: 0\n", wantInterval: 0},
		{name: "string zero disables", cluster: "  detectInterval: \"0\"\n", wantInterval: 0},
		{name: "seconds above minimum", cluster: "  detectInterval: 90s\n", wantInterval: 90 * time.Second},
		{name: "minimum interval", cluster: "  detectInterval: 1m\n", wantInterval: time.Minute},
		{name: "below minimum", cluster: "  detectInterval: 30s\n", wantErr: "must be 0 (disabled) or at least 1m0s"},
		{name: "negative", cluster: "  detectInterval: -1h\n", wantErr: "detectInterval"},
		{name: "malformed string", cluster: "  detectInterval: hourly\n", wantErr: `invalid duration "hourly"`},
		{name: "empty string", cluster: "  detectInterval: \"\"\n", wantErr: "invalid duration"},
		{name: "non-zero bare integer", cluster: "  detectInterval: 3600\n", wantErr: "duration 3600 has no unit"},
		{name: "mapping value", cluster: "  detectInterval: {h: 1}\n", wantErr: "must be a scalar"},
		{
			name:         "valid rules",
			cluster:      "  detectID: true\n  id: my-id\n  distributions:\n    - match: '\\+my-distro'\n      name: my-distro.v2\n",
			wantInterval: DefaultDetectInterval,
			wantRules:    1,
		},
		{name: "invalid regex", cluster: "  distributions:\n    - {match: '(unclosed', name: x}\n", wantErr: "not a valid regular expression"},
		{name: "empty match", cluster: "  distributions:\n    - {match: '', name: x}\n", wantErr: "match is required"},
		{name: "uppercase name", cluster: "  distributions:\n    - {match: 'x', name: MyDistro}\n", wantErr: "name"},
		{name: "empty name", cluster: "  distributions:\n    - {match: 'x', name: ''}\n", wantErr: "name"},
		{name: "name too long", cluster: "  distributions:\n    - {match: 'x', name: " + strings.Repeat("a", 64) + "}\n", wantErr: "name"},
		{name: "too many rules", cluster: manyRules.String(), wantErr: "at most 20"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(base + tc.cluster + tail))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error: %v", err)
			}
			if got := cfg.Cluster.DetectIntervalOrDefault(); got != tc.wantInterval {
				t.Errorf("DetectInterval = %s, want %s", got, tc.wantInterval)
			}
			if len(cfg.Cluster.Distributions) != tc.wantRules {
				t.Errorf("Distributions = %v, want %d rules", cfg.Cluster.Distributions, tc.wantRules)
			}
		})
	}
}

func TestClusterIDEnvOverride(t *testing.T) {
	t.Setenv("TUBER_CLUSTER_ID", "  env-cluster-id ")
	cfg, err := Parse([]byte("cluster:\n  name: edge\n  provider: self-hosted\n  id: file-id\nwatchNamespaces: [default]\ndryRun: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cluster.ID != "env-cluster-id" {
		t.Errorf("Cluster.ID = %q, want env-cluster-id", cfg.Cluster.ID)
	}
}

func TestOfflineValidationUsesSampleClusterValues(t *testing.T) {
	t.Setenv("TUBER_CLUSTER_ID", "")
	tmpl := "{{.Distribution}}/{{.ClusterVersion}}/{{.ClusterID}}"
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "cluster:\n  name: edge\n  provider: self-hosted\nwatchNamespaces: [default]\ndoit:\n  contentTemplate: '" + tmpl + "'\n  serviceAccountTokenFile: /nonexistent/token\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPreview(path); err != nil {
		t.Errorf("LoadPreview() rejected new template variables: %v", err)
	}
	if _, err := LoadValidation(path); err != nil {
		t.Errorf("LoadValidation() rejected new template variables: %v", err)
	}
	if SampleClusterVersion != "v1.31.0" || SampleDistribution != "k3s" || SampleClusterID != "sample-cluster-id" {
		t.Errorf("sample values changed: %s %s %s", SampleClusterVersion, SampleDistribution, SampleClusterID)
	}
}
