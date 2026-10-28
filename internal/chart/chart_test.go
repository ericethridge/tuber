package chart_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericethridge/tuber/internal/config"
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
	// clusterRoles and clusterBindings are rendered only for the opt-in cluster.detectID.
	clusterRoles    []rbacv1.ClusterRole
	clusterBindings []rbacv1.ClusterRoleBinding
	kinds           []string
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
		case "ClusterRole":
			var role rbacv1.ClusterRole
			err = sigyaml.Unmarshal(raw, &role)
			result.clusterRoles = append(result.clusterRoles, role)
		case "ClusterRoleBinding":
			var binding rbacv1.ClusterRoleBinding
			err = sigyaml.Unmarshal(raw, &binding)
			result.clusterBindings = append(result.clusterBindings, binding)
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
		{"uppercase provider", []string{"--set", "cluster.provider=Self-Hosted"}},
		{"underscore provider", []string{"--set", "cluster.provider=gcp_gke"}},
		{"provider with leading dash", []string{"--set", "cluster.provider=-gke"}},
		{"provider too long", []string{"--set", "cluster.provider=" + strings.Repeat("a", 64)}},
		{"empty provider", []string{"--set", "cluster.provider="}},
		{"interval below one minute", []string{"--set", "cluster.detectInterval=30s"}},
		{"zero-minute interval", []string{"--set", "cluster.detectInterval=0m"}},
		{"malformed interval", []string{"--set", "cluster.detectInterval=hourly"}},
		{"empty interval", []string{"--set-string", "cluster.detectInterval="}},
		{"string detectID", []string{"--set-string", "cluster.detectID=yes"}},
		{"distribution without name", []string{"--set-json", `cluster.distributions=[{"match":"x"}]`}},
		{"distribution without match", []string{"--set-json", `cluster.distributions=[{"name":"x"}]`}},
		{"distribution empty match", []string{"--set-json", `cluster.distributions=[{"match":"","name":"x"}]`}},
		{"uppercase distribution name", []string{"--set-json", `cluster.distributions=[{"match":"x","name":"MyDistro"}]`}},
		{"distribution name too long", []string{"--set-json", `cluster.distributions=[{"match":"x","name":"` + strings.Repeat("a", 64) + `"}]`}},
		{"unknown distribution key", []string{"--set-json", `cluster.distributions=[{"match":"x","name":"x","priority":1}]`}},
		{"too many distributions", []string{"--set-json", "cluster.distributions=[" + strings.TrimSuffix(strings.Repeat(`{"match":"x","name":"x"},`, 21), ",") + "]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := render(t, "gke.yaml", tc.args...); err == nil {
				t.Fatal("invalid values were accepted")
			}
		})
	}
}

func TestChartAcceptsFreeFormLowercaseProviders(t *testing.T) {
	for _, provider := range []string{"gcp-gke", "aws-eks", "azure-aks", "self-hosted", "other", "k3s-edge", "openshift4"} {
		t.Run(provider, func(t *testing.T) {
			r, err := render(t, "gke.yaml", "--set", "cluster.provider="+provider)
			if err != nil {
				t.Fatal(err)
			}
			if got := loadRenderedConfig(t, r).Cluster.Provider; got != provider {
				t.Fatalf("provider = %q, want %q", got, provider)
			}
		})
	}
}

func TestChartClusterRoleOnlyForOptInClusterID(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"default", nil},
		{"explicit id without detection", []string{"--set", "cluster.id=my-cluster-id"}},
		{"detection without chart RBAC", []string{"--set", "cluster.detectID=true", "--set", "rbac.create=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := render(t, "gke.yaml", tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			for _, kind := range r.kinds {
				if strings.HasPrefix(kind, "Cluster") {
					t.Errorf("render must not contain cluster-scoped %s", kind)
				}
			}
		})
	}

	r, err := render(t, "gke.yaml", "--set", "cluster.detectID=true")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.clusterRoles) != 1 || len(r.clusterBindings) != 1 {
		t.Fatalf("detectID must add exactly one ClusterRole and ClusterRoleBinding, got kinds %v", r.kinds)
	}
	role, binding := r.clusterRoles[0], r.clusterBindings[0]
	if len(role.Rules) != 1 || role.AggregationRule != nil {
		t.Fatalf("ClusterRole must have a single rule and no aggregation: %+v", role)
	}
	rule := role.Rules[0]
	if len(rule.APIGroups) != 1 || rule.APIGroups[0] != "" ||
		strings.Join(rule.Resources, ",") != "namespaces" ||
		strings.Join(rule.ResourceNames, ",") != "kube-system" ||
		strings.Join(rule.Verbs, ",") != "get" || len(rule.NonResourceURLs) != 0 {
		t.Fatalf("ClusterRole must allow only get on the kube-system namespace: %+v", rule)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != role.Name {
		t.Fatalf("binding must reference the release ClusterRole: %+v", binding.RoleRef)
	}
	sa := r.deployment.Spec.Template.Spec.ServiceAccountName
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" ||
		binding.Subjects[0].Name != sa || binding.Subjects[0].Namespace != "monitoring" {
		t.Fatalf("binding must target only the release ServiceAccount: %+v", binding.Subjects)
	}

	other, err := render(t, "gke.yaml", "--set", "cluster.detectID=true", "--namespace", "other")
	if err != nil {
		t.Fatal(err)
	}
	if other.clusterRoles[0].Name == role.Name {
		t.Fatal("cluster-scoped names must be unique per release namespace and name")
	}
}

func TestChartRendersClusterDetectionConfig(t *testing.T) {
	r, err := render(t, "self-hosted.yaml", "--set", "cluster.id=my-cluster-id",
		"--set", "cluster.detectInterval=30m",
		"--set-json", `cluster.distributions=[{"match":"\\+my-distro","name":"my-distro"}]`)
	if err != nil {
		t.Fatal(err)
	}
	c := loadRenderedConfig(t, r).Cluster
	if c.Provider != "self-hosted" || c.ID != "my-cluster-id" || !c.DetectID || c.DetectIntervalOrDefault() != 30*time.Minute {
		t.Fatalf("cluster detection settings were not rendered: %+v", c)
	}
	if len(c.Distributions) != 1 || c.Distributions[0].Match != `\+my-distro` || c.Distributions[0].Name != "my-distro" {
		t.Fatalf("distribution rule was not rendered: %+v", c.Distributions)
	}

	disabled, err := render(t, "gke.yaml", "--set", "cluster.detectInterval=0")
	if err != nil {
		t.Fatal(err)
	}
	if got := loadRenderedConfig(t, disabled).Cluster.DetectIntervalOrDefault(); got != 0 {
		t.Fatalf("detectInterval=0 must disable rechecks, got %v", got)
	}

	def, err := render(t, "gke.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id:", "detectID:", "detectInterval:", "distributions:"} {
		if strings.Contains(def.configMap.Data["config.yaml"], key) {
			t.Errorf("default config must omit unset %s", key)
		}
	}
	if got := loadRenderedConfig(t, def).Cluster.DetectIntervalOrDefault(); got != time.Hour {
		t.Fatalf("default interval = %v, want 1h", got)
	}

	// The schema cannot compile regexes; the runtime validator must reject them.
	broken, err := render(t, "gke.yaml", "--set-json", `cluster.distributions=[{"match":"(","name":"broken"}]`)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(broken.configMap.Data["config.yaml"]), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadValidation(path); err == nil {
		t.Fatal("runtime must reject a distribution rule whose regex does not compile")
	}
}

func loadRenderedConfig(t *testing.T, r renderedChart) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(r.configMap.Data["config.yaml"]), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadValidation(path)
	if err != nil {
		t.Fatalf("runtime rejected rendered config: %v", err)
	}
	return cfg
}
