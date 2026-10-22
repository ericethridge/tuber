package formatter

import (
	"reflect"
	"testing"
	"time"

	"github.com/ericeng/tuber/internal/config"
	"github.com/ericeng/tuber/internal/model"
)

func TestFormatterRendersTemplateAndMergesMetadata(t *testing.T) {
	f, err := New(config.DoiTConfig{
		ContentTemplate: config.DefaultContentTemplate,
		DefaultReports:  []string{"rep-global"},
		DefaultLabels:   []string{"lbl-global"},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ts := time.Date(2026, 10, 21, 14, 30, 5, 123000000, time.UTC)
	ev := model.WorkloadEvent{
		ClusterName:   "gke-prod-1",
		CloudProvider: "gcp-gke",
		Region:        "us-central1",
		Namespace:     "checkout",
		Kind:          "Deployment",
		APIVersion:    "apps/v1",
		Name:          "payment-worker",
		Action:        model.ActionRolledOut,
		Summary:       "image=gcr.io/acme/pay:v1.4.2 replicas=6",
		Timestamp:     ts,
		RuleReportIDs: []string{"rep-rule", "rep-global"},
		ReportIDs:     []string{"rep-k8s-annotation"},
		RuleLabelIDs:  []string{"lbl-rule"},
		LabelIDs:      []string{"lbl-k8s-annotation"},
	}

	req, err := f.Format(ev)
	if err != nil {
		t.Fatalf("Format() error: %v", err)
	}

	wantContent := "[gcp-gke/gke-prod-1] checkout/Deployment/payment-worker RolledOut: image=gcr.io/acme/pay:v1.4.2 replicas=6"
	if req.Content != wantContent {
		t.Errorf("Content = %q, want %q", req.Content, wantContent)
	}
	if req.Timestamp != "2026-10-21T14:30:05.123Z" {
		t.Errorf("Timestamp = %q, want 2026-10-21T14:30:05.123Z", req.Timestamp)
	}

	wantReports := []string{"rep-global", "rep-rule", "rep-k8s-annotation"}
	if !reflect.DeepEqual(req.Reports, wantReports) {
		t.Errorf("Reports = %v, want %v", req.Reports, wantReports)
	}

	wantLabels := []string{"lbl-global", "lbl-rule", "lbl-k8s-annotation"}
	if !reflect.DeepEqual(req.Labels, wantLabels) {
		t.Errorf("Labels = %v, want %v", req.Labels, wantLabels)
	}
}
