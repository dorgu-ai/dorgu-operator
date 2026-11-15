package chart

import (
	"os"
	"strings"
	"testing"
)

// C6b (review F-3): how long a value a person rolled back stays withheld is a chart
// value, remediation.rolledBackValueTTL, rendered as --rolled-back-value-ttl and 168h
// (7 days) by default. No CRD changes: the lifetime is counted from the rollback
// action's own completion.

func TestRolledBackValueTTL_RendersTheDefault(t *testing.T) {
	mustContain(t, helmTemplate(t), "--rolled-back-value-ttl=168h", "fresh install")
}

func TestRolledBackValueTTL_ValueIsRendered(t *testing.T) {
	out := helmTemplate(t, "remediation.rolledBackValueTTL=48h")
	mustContain(t, out, "--rolled-back-value-ttl=48h", "remediation.rolledBackValueTTL=48h")
}

func TestRolledBackValueTTL_IsDocumentedInValuesYaml(t *testing.T) {
	raw, err := os.ReadFile("values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "rolledBackValueTTL: 168h") {
		t.Error("values.yaml must declare remediation.rolledBackValueTTL: 168h")
	}
}

func TestRolledBackValueTTL_RendersAgainstTheOldValues(t *testing.T) {
	out, err := renderAgainstValues(t)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	mustContain(t, out, "--rolled-back-value-ttl=168h", "0.12.1 values, new chart")
}

// The chart refuses exactly what the operator refuses: a Go duration, positive, at most
// 2160h (90 days).
func TestRolledBackValueTTL_RefusesWhatTheOperatorRefuses(t *testing.T) {
	cases := map[string][]string{
		"zero (a bare number)":    {"--set", "remediation.rolledBackValueTTL=0"},
		"empty":                   {"--set-string", "remediation.rolledBackValueTTL="},
		"a bare number":           {"--set-string", "remediation.rolledBackValueTTL=168"},
		"zero hours":              {"--set-string", "remediation.rolledBackValueTTL=0h"},
		"negative":                {"--set-string", "remediation.rolledBackValueTTL=-24h"},
		"over 90 days":            {"--set-string", "remediation.rolledBackValueTTL=2161h"},
		"days are not a unit":     {"--set-string", "remediation.rolledBackValueTTL=7d"},
		"not a duration":          {"--set-string", "remediation.rolledBackValueTTL=banana"},
		"a value that is a list":  {"--set", "remediation.rolledBackValueTTL={48h}"},
		"a duration plus junk":    {"--set-string", "remediation.rolledBackValueTTL=48hx"},
		"a duration with a space": {"--set-string", "remediation.rolledBackValueTTL=48 h"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := helmTemplateArgs(t, args...)
			if err == nil {
				t.Fatalf("helm template accepted it and rendered:\n%s", grepLine(out, "--rolled-back-value-ttl"))
			}
			if !strings.Contains(out, "remediation.rolledBackValueTTL") {
				t.Errorf("the failure must name the value:\n%s", out)
			}
		})
	}
}

func TestRolledBackValueTTL_AcceptsWhatTheOperatorAccepts(t *testing.T) {
	for _, ttl := range []string{"168h", "48h", "1h", "90m", "2160h", "129600m", "36h30m"} {
		t.Run(ttl, func(t *testing.T) {
			out, err := helmTemplateArgs(t, "--set-string", "remediation.rolledBackValueTTL="+ttl)
			if err != nil {
				t.Fatalf("helm template refused %q: %v\n%s", ttl, err, out)
			}
			mustContain(t, out, "--rolled-back-value-ttl="+ttl, ttl)
		})
	}
}
