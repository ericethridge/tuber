package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericeng/tuber/internal/config"
	"gopkg.in/yaml.v3"
)

func TestConfigPreviewOffline(t *testing.T) {
	t.Setenv("TUBER_CLUSTER_NAME", "override-cluster")
	t.Setenv("TUBER_WATCH_NAMESPACES", "payments,default,payments")
	t.Setenv("DOIT_SERVICE_ACCOUNT_TOKEN", "secret-env-token")
	t.Setenv("DOIT_API_TOKEN", "secret-legacy-token")
	t.Setenv("DOIT_TENANT_ID", "secret-tenant")
	t.Setenv("TUBER_DRY_RUN", "true")
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
cluster:
  name: file-cluster
  provider: gcp-gke
watchNamespaces: [default]
doit:
  serviceAccountToken: secret-file-token
  apiToken: secret-file-legacy-token
  serviceAccountTokenFile: /secret/credential-path
  apiTokenFile: /secret/legacy-path
  baseURL: https://secret-url-user:secret-url-password@example.com
  contentTemplate: "{{.ClusterName}}/{{.Namespace}} {{.Name}} {{.Action}}"
  defaultReports: [report-1]
rules:
  - name: deployments
    apiVersion: apps/v1
    kind: Deployment
    actions: [Scaled]
    labelSelector: team=payments
`), 0600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadPreview(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeConfigPreview(&out, cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "secret-") || strings.Contains(out.String(), "/secret/") {
		t.Fatalf("preview exposed authentication metadata: %s", out.String())
	}
	var preview configPreview
	if err := yaml.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatalf("preview is not valid YAML: %v", err)
	}
	if preview.SampleAnnotation != "override-cluster/payments sample-app Created" {
		t.Errorf("sample annotation = %q", preview.SampleAnnotation)
	}
	if len(preview.WatchNamespaces) != 2 || !preview.DryRun {
		t.Errorf("effective overrides not reflected: %+v", preview)
	}
	if preview.Rollup.Workers != config.DefaultWorkerCount || !preview.Rollup.IgnoreOwnedPodsEnabled() {
		t.Errorf("effective defaults not reflected: %+v", preview.Rollup)
	}
	if len(preview.Rules) != 1 || preview.Rules[0].LabelSelector != "team=payments" || len(preview.DefaultReports) != 1 {
		t.Errorf("routing settings not reflected: %+v", preview)
	}
	if !strings.Contains(preview.SampleNote, "not simulated") {
		t.Error("sample must explain that routing is not simulated")
	}
	if err := writeConfigPreview(failingWriter{}, cfg); err == nil || !strings.Contains(err.Error(), errPreviewWrite.Error()) {
		t.Errorf("write failure = %v, want %v", err, errPreviewWrite)
	}
}

var errPreviewWrite = errors.New("preview output unavailable")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errPreviewWrite }
