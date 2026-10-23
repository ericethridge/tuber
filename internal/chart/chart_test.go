package chart_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericeng/tuber/internal/config"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	sigyaml "sigs.k8s.io/yaml"
)

const chartPath = "../../charts/tuber"

type renderedChart struct {
	deployment appsv1.Deployment
	configMap  corev1.ConfigMap
	roles      []rbacv1.Role
	bindings   []rbacv1.RoleBinding
	kinds      []string
}

func render(t *testing.T, example string, extra ...string) (renderedChart, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart tests (installed by CI)")
	}
	args := []string{"template", "smoke", chartPath, "--namespace", "monitoring", "-f", filepath.Join(chartPath, "examples", example)}
	out, err := exec.Command(helm, append(args, extra...)...).CombinedOutput()
	if err != nil {
		return renderedChart{}, &renderError{output: string(out)}
	}
	var result renderedChart
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(doc) == 0 {
			continue
		}
		raw, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		kind, _ := doc["kind"].(string)
		result.kinds = append(result.kinds, kind)
		switch kind {
		case "Deployment":
			err = sigyaml.Unmarshal(raw, &result.deployment)
		case "ConfigMap":
			err = sigyaml.Unmarshal(raw, &result.configMap)
		case "Role":
			var role rbacv1.Role
			err = sigyaml.Unmarshal(raw, &role)
			result.roles = append(result.roles, role)
		case "RoleBinding":
			var binding rbacv1.RoleBinding
			err = sigyaml.Unmarshal(raw, &binding)
			result.bindings = append(result.bindings, binding)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return result, nil
}

type renderError struct{ output string }

func (e *renderError) Error() string { return e.output }

func TestChartRendersRuntimeConfigAndScopedAccess(t *testing.T) {
	chartRaw, err := os.ReadFile(filepath.Join(chartPath, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(chartRaw, &metadata); err != nil {
		t.Fatal(err)
	}
	for _, example := range []string{"gke.yaml", "eks.yaml"} {
		t.Run(example, func(t *testing.T) {
			r, err := render(t, example, "--set-json", `watchNamespaces=["default","payments"]`,
				"--set", "credentials.tokenKey=api-token")
			if err != nil {
				t.Fatal(err)
			}
			if len(r.kinds) != 9 {
				t.Fatalf("rendered kinds = %v; expected Deployment, ConfigMap, ServiceAccount and three RBAC pairs", r.kinds)
			}
			for _, kind := range r.kinds {
				if kind == "Secret" || strings.HasPrefix(kind, "ClusterRole") || kind == "Namespace" {
					t.Errorf("chart must not own %s", kind)
				}
			}
			if r.deployment.Namespace != "monitoring" || r.deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || *r.deployment.Spec.Replicas != 1 {
				t.Fatal("single-watcher upgrades must stop the old pod before starting the new one")
			}
			pod := r.deployment.Spec.Template.Spec
			if pod.Containers[0].Image != "ghcr.io/ericethridge/tuber:v"+metadata.AppVersion {
				t.Errorf("unexpected release image: %s", pod.Containers[0].Image)
			}
			secret := pod.Volumes[1].Secret
			if secret.SecretName != "tuber-doit-credentials" || secret.Items[0].Key != "api-token" || secret.Items[0].Path != "service-account-token" {
				t.Fatalf("credential mount must map the external token key to the runtime path: %+v", secret)
			}
			for _, role := range r.roles {
				if role.Namespace != "monitoring" && role.Namespace != "default" && role.Namespace != "payments" {
					t.Errorf("role grants access to unwatched namespace %q", role.Namespace)
				}
				if role.Namespace != "monitoring" {
					for _, rule := range role.Rules {
						if strings.Join(rule.Verbs, ",") != "get,list,watch" {
							t.Errorf("workload access must be read-only: %v", rule.Verbs)
						}
					}
				}
			}
			for _, binding := range r.bindings {
				if binding.Subjects[0].Namespace != "monitoring" || binding.Subjects[0].Name != pod.ServiceAccountName {
					t.Errorf("binding does not target release ServiceAccount: %+v", binding)
				}
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(r.configMap.Data["config.yaml"]), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadValidation(path)
			if err != nil {
				t.Fatalf("runtime rejected rendered config: %v", err)
			}
			if len(cfg.Rules) != 3 || len(cfg.WatchNamespaces) != 2 || !cfg.DoiT.ValidateOnStartupEnabled() {
				t.Fatal("runtime defaults or authentication checks were lost")
			}
		})
	}
}

func TestChartConfigChangesRestartPodAndSupportExistingServiceAccount(t *testing.T) {
	base, err := render(t, "gke.yaml")
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	changed, err := render(t, "gke.yaml", "--set", "doit.defaultReports[0]=report-1",
		"--set", "serviceAccount.create=false", "--set", "serviceAccount.name=existing-tuber",
		"--set", "image.digest="+digest, "--set", "credentials.tenantIdKey=", "--is-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if base.deployment.Spec.Template.Annotations["checksum/config"] == changed.deployment.Spec.Template.Annotations["checksum/config"] {
		t.Fatal("config changes must change the pod template checksum")
	}
	if changed.deployment.Spec.Template.Spec.ServiceAccountName != "existing-tuber" {
		t.Fatal("existing ServiceAccount was not selected")
	}
	for _, kind := range changed.kinds {
		if kind == "ServiceAccount" {
			t.Fatal("chart must not create an existing ServiceAccount")
		}
	}
	container := changed.deployment.Spec.Template.Spec.Containers[0]
	if container.Image != "ghcr.io/ericethridge/tuber@"+digest || len(container.Env) != 0 {
		t.Fatal("digest must replace tag; empty tenant key must omit Secret env reference")
	}
}

func TestChartRejectsUnsafeOrIncompleteValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing cluster", []string{"--set", "cluster.name="}},
		{"missing secret", []string{"--set", "credentials.existingSecret="}},
		{"inline secret", []string{"--set", "doit.serviceAccountToken=must-not-render"}},
		{"wildcard namespace", []string{"--set-json", `watchNamespaces=["*"]`}},
		{"all namespaces", []string{"--set-json", `watchNamespaces=["all"]`}},
		{"empty namespaces", []string{"--set-json", `watchNamespaces=[]`}},
		{"duplicate namespaces", []string{"--set-json", `watchNamespaces=["default","default"]`}},
		{"missing existing account", []string{"--set", "serviceAccount.create=false"}},
		{"malformed digest", []string{"--set", "image.digest=sha256:abc:latest"}},
		{"tag in repository", []string{"--set", "image.repository=ghcr.io/example/tuber:latest"}},
		{"wrong resource version", []string{"--set-json", `rules=[{"kind":"Job","apiVersion":"apps/v1"}]`}},
		{"unwatched rule namespace", []string{"--set-json", `rules=[{"kind":"Job","apiVersion":"batch/v1","namespaces":["unwatched"]}]`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := render(t, "gke.yaml", tc.args...); err == nil {
				t.Fatal("invalid values were accepted")
			}
		})
	}
}
