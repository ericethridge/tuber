package main

import (
	"fmt"
	"io"

	"github.com/ericeng/tuber/internal/config"
	"github.com/ericeng/tuber/internal/formatter"
	"github.com/ericeng/tuber/internal/model"
	"gopkg.in/yaml.v3"
)

// Keep preview fields explicit so new authentication settings cannot be exposed
// accidentally by serializing the runtime configuration.
type configPreview struct {
	Note              string               `yaml:"note"`
	Cluster           config.ClusterConfig `yaml:"cluster"`
	WatchNamespaces   []string             `yaml:"watchNamespaces"`
	DryRun            bool                 `yaml:"dryRun"`
	Rollup            config.RollupConfig  `yaml:"rollup"`
	Rules             []config.WatchRule   `yaml:"rules"`
	DefaultReports    []string             `yaml:"defaultReports"`
	DefaultLabels     []string             `yaml:"defaultLabels"`
	ValidateOnStartup bool                 `yaml:"validateOnStartup"`
	RejectUserTokens  bool                 `yaml:"rejectUserTokens"`
	SampleAnnotation  string               `yaml:"sampleAnnotation"`
	SampleNote        string               `yaml:"sampleNote"`
}

func writeConfigPreview(w io.Writer, cfg *config.Config) error {
	f, err := formatter.New(cfg.DoiT)
	if err != nil {
		return err
	}
	payload, err := f.Format(model.WorkloadEvent{
		ClusterName: cfg.Cluster.Name, CloudProvider: cfg.Cluster.Provider,
		Region: cfg.Cluster.Region, Namespace: cfg.WatchNamespaces[0],
		APIVersion: "apps/v1", Kind: "Deployment", Name: "sample-app",
		UID: "sample-uid", Action: model.ActionCreated,
		Summary: "replicas=3 images=app=example:v1",
	})
	if err != nil {
		return fmt.Errorf("formatting preview annotation: %w", err)
	}
	preview := configPreview{
		Note:    "Offline preview after defaults and environment overrides. Credentials, credential paths, tenant ID, and API URL are excluded. Cluster access, authentication, and report permissions are not checked.",
		Cluster: cfg.Cluster, WatchNamespaces: cfg.WatchNamespaces, DryRun: cfg.DryRun,
		Rollup: cfg.Rollup, Rules: cfg.Rules,
		DefaultReports: cfg.DoiT.DefaultReports, DefaultLabels: cfg.DoiT.DefaultLabels,
		ValidateOnStartup: cfg.DoiT.ValidateOnStartupEnabled(),
		RejectUserTokens:  cfg.DoiT.RejectUserTokensEnabled(),
		SampleAnnotation:  payload.Content,
		SampleNote:        "Synthetic Deployment Created example for template formatting only. Rules are listed in first-match order; selectors, rule routing, and workload overrides are not simulated.",
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(preview)
}
