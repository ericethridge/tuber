package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildKubeConfigUsesMergedEnvironmentFiles(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	dir := t.TempDir()
	contextPath := filepath.Join(dir, "context.yaml")
	credentialsPath := filepath.Join(dir, "credentials.yaml")
	for path, content := range map[string]string{
		contextPath: `apiVersion: v1
kind: Config
current-context: test
contexts:
  - name: test
    context:
      cluster: test
      user: test
clusters:
  - name: test
    cluster:
      server: https://example.invalid
`,
		credentialsPath: `apiVersion: v1
kind: Config
users:
  - name: test
    user:
      token: test-kubernetes-token
`,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KUBECONFIG", contextPath+string(os.PathListSeparator)+credentialsPath)
	cfg, err := buildKubeConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://example.invalid" || cfg.BearerToken != "test-kubernetes-token" {
		t.Fatalf("standard kubeconfig loading did not merge context and credentials")
	}
	if _, err := buildKubeConfig(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("explicit kubeconfig must take precedence over KUBECONFIG")
	}
}
