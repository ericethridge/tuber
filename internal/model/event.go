package model

import (
	"fmt"
	"strings"
	"time"
)

// Action represents a high-level workload lifecycle event type.
type Action string

const (
	ActionCreated   Action = "Created"
	ActionScaled    Action = "Scaled"
	ActionRolledOut Action = "RolledOut"
	ActionDeleted   Action = "Deleted"
	ActionCompleted Action = "Completed"
	ActionFailed    Action = "Failed"
	ActionOOMKilled Action = "OOMKilled"
)

// Kubernetes resource annotations supported by tuber for per-workload overrides.
const (
	AnnotationIgnore  = "tuber.doit.com/ignore"
	AnnotationWatch   = "tuber.doit.com/watch"
	AnnotationReports = "tuber.doit.com/reports"
	AnnotationLabels  = "tuber.doit.com/labels"

	// MaxOverrideIDs caps the number of DoiT report or label IDs extracted per resource or request.
	MaxOverrideIDs = 20
	// MaxIDLength caps the length of a single DoiT report or label ID.
	MaxIDLength = 64
	// MaxAnnotationInputLength caps the raw K8s annotation value length inspected by splitCSV.
	MaxAnnotationInputLength = 2048
)

// WorkloadEvent represents a normalized, actionable Kubernetes resource lifecycle change
// ready to be formatted and sent to the DoiT Cloud Intelligence Annotations API.
type WorkloadEvent struct {
	ClusterName   string    `json:"clusterName"`
	CloudProvider string    `json:"cloudProvider"`
	Region        string    `json:"region,omitempty"`
	Namespace     string    `json:"namespace"`
	Kind          string    `json:"kind"`
	APIVersion    string    `json:"apiVersion"`
	Name          string    `json:"name"`
	UID           string    `json:"uid"`
	Action        Action    `json:"action"`
	Summary       string    `json:"summary"`
	Timestamp     time.Time `json:"timestamp"`
	Generation    int64     `json:"generation"`
	StateKey      string    `json:"stateKey,omitempty"`

	// Per-resource overrides from K8s annotations (tuber.doit.com/reports, tuber.doit.com/labels).
	ReportIDs []string `json:"reportIds,omitempty"`
	LabelIDs  []string `json:"labelIds,omitempty"`

	// Rule-level overrides from the matched WatchRule.
	RuleReportIDs []string `json:"ruleReportIds,omitempty"`
	RuleLabelIDs  []string `json:"ruleLabelIds,omitempty"`
}

// Fingerprint returns a deterministic deduplication key for this event transition.
func (e WorkloadEvent) Fingerprint() string {
	return fmt.Sprintf("%s:%s:%s:%s:%d:%s", e.Namespace, e.Kind, e.UID, e.Action, e.Generation, e.StateKey)
}

// AnnotationOverrides holds per-resource metadata extracted from Kubernetes object annotations.
type AnnotationOverrides struct {
	Reports    []string
	Labels     []string
	Ignore     bool
	ForceWatch bool
}

// ExtractAnnotationOverrides parses tuber.doit.com/* annotations from a Kubernetes object's metadata.
func ExtractAnnotationOverrides(annotations map[string]string) AnnotationOverrides {
	if len(annotations) == 0 {
		return AnnotationOverrides{}
	}

	return AnnotationOverrides{
		Reports:    splitCSV(annotations[AnnotationReports]),
		Labels:     splitCSV(annotations[AnnotationLabels]),
		Ignore:     isTruthy(annotations[AnnotationIgnore]),
		ForceWatch: isTruthy(annotations[AnnotationWatch]),
	}
}

func splitCSV(raw string) []string {
	if len(raw) > MaxAnnotationInputLength {
		raw = raw[:MaxAnnotationInputLength]
	}
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if !IsValidDoiTID(trimmed) {
			continue
		}
		if _, exists := seen[trimmed]; !exists {
			seen[trimmed] = struct{}{}
			out = append(out, trimmed)
			if len(out) >= MaxOverrideIDs {
				break
			}
		}
	}
	return out
}

// IsValidDoiTID validates that a DoiT report or label identifier consists strictly of
// 1..64 ASCII alphanumeric, hyphen, or underscore characters.
func IsValidDoiTID(id string) bool {
	if len(id) == 0 || len(id) > MaxIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlphaNum && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// SanitizeText strips ASCII control characters (replacing newlines/tabs with single spaces)
// and caps the string at maxRunes runes.
func SanitizeText(s string, maxRunes int) string {
	var b strings.Builder
	b.Grow(len(s))
	count := 0
	for _, r := range s {
		if maxRunes > 0 && count >= maxRunes {
			break
		}
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteByte(' ')
			count++
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String())
}

func isTruthy(val string) bool {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// MergeUniqueStrings merges multiple string slices preserving order, validating DoiT ID format,
// removing duplicates/blanks, and capping at MaxOverrideIDs.
func MergeUniqueStrings(slices ...[]string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, s := range slices {
		for _, item := range s {
			trimmed := strings.TrimSpace(item)
			if !IsValidDoiTID(trimmed) {
				continue
			}
			if _, ok := seen[trimmed]; !ok {
				seen[trimmed] = struct{}{}
				out = append(out, trimmed)
				if len(out) >= MaxOverrideIDs {
					return out
				}
			}
		}
	}
	return out
}
