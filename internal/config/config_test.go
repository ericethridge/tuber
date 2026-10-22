package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericethridge/tuber/internal/model"
)

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
