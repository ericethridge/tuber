package formatter

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/ericeng/tuber/internal/config"
	"github.com/ericeng/tuber/internal/doit"
	"github.com/ericeng/tuber/internal/model"
)

// Formatter transforms normalized WorkloadEvents into DoiT CreateAnnotationRequest payloads.
type Formatter struct {
	tmpl           *template.Template
	defaultReports []string
	defaultLabels  []string
}

// New constructs a Formatter from the DoiT configuration section.
func New(cfg config.DoiTConfig) (*Formatter, error) {
	rawTmpl := strings.TrimSpace(cfg.ContentTemplate)
	if rawTmpl == "" {
		rawTmpl = config.DefaultContentTemplate
	}
	tmpl, err := template.New("annotation").Option("missingkey=error").Parse(rawTmpl)
	if err != nil {
		return nil, fmt.Errorf("parsing annotation template: %w", err)
	}
	return &Formatter{
		tmpl:           tmpl,
		defaultReports: cfg.DefaultReports,
		defaultLabels:  cfg.DefaultLabels,
	}, nil
}

// Format renders a WorkloadEvent into a DoiT CreateAnnotationRequest, combining
// global defaults, rule-level overrides, and per-resource Kubernetes annotation overrides.
func (f *Formatter) Format(event model.WorkloadEvent) (doit.CreateAnnotationRequest, error) {
	var buf bytes.Buffer
	if err := f.tmpl.Execute(&buf, event); err != nil {
		return doit.CreateAnnotationRequest{}, fmt.Errorf("executing annotation template: %w", err)
	}

	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	} else {
		ts = ts.UTC()
	}

	reports := model.MergeUniqueStrings(f.defaultReports, event.RuleReportIDs, event.ReportIDs)
	labels := model.MergeUniqueStrings(f.defaultLabels, event.RuleLabelIDs, event.LabelIDs)

	return doit.CreateAnnotationRequest{
		Content:   strings.TrimSpace(buf.String()),
		Timestamp: ts.Format("2006-01-02T15:04:05.000Z"),
		Reports:   reports,
		Labels:    labels,
	}, nil
}
