package integration

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// chartDir resolves the on-call UI chart relative to this test file.
// The test runs from tests/oncall_ui/integration/, so the chart is
// three levels up.
func chartDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "deployment", "helm", "nodemedic-oncall-ui")
}

// helmTemplate runs `helm template` against the chart with the given
// extra args, returning combined output.
func helmTemplate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH: %v", err)
	}
	full := append([]string{"template", "nodemedic-oncall-ui", chartDir(t)}, args...)
	cmd := exec.Command("helm", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestChartGuardAcceptsAllowedClusters verifies the cluster-name
// allowlist (Constitution Article I.5 + FR-23): cf1z, jc1z, sk1z, and
// any test-* prefix MUST render successfully.
func TestChartGuardAcceptsAllowedClusters(t *testing.T) {
	for _, name := range []string{"cf1z", "jc1z", "sk1z", "test-foo"} {
		t.Run(name, func(t *testing.T) {
			out, err := helmTemplate(t, "--set", "clusterName="+name)
			if err != nil {
				t.Fatalf("helm template clusterName=%s: %v\n%s", name, err, out)
			}
			if !strings.Contains(out, "kind: ClusterRole") {
				t.Errorf("clusterName=%s: rendered output missing ClusterRole", name)
			}
		})
	}
}

// TestChartGuardRejectsProductionClusters verifies the cluster-name
// guard fails the chart render for production-shaped names. Each
// rejection case MUST exit non-zero AND name the constitution article
// in the error so an operator running into the failure understands why.
func TestChartGuardRejectsProductionClusters(t *testing.T) {
	cases := []string{
		"",
		"stg-going-plaid",
		"us-big-cone",
		"eu-lesser-forest",
		"production-cluster",
	}
	for _, name := range cases {
		t.Run("reject_"+name, func(t *testing.T) {
			args := []string{}
			if name != "" {
				args = []string{"--set", "clusterName=" + name}
			}
			out, err := helmTemplate(t, args...)
			if err == nil {
				t.Fatalf("clusterName=%q: expected helm template to fail; got success:\n%s", name, out)
			}
			if !strings.Contains(out, "Article I.5") {
				t.Errorf("clusterName=%q: error output missing 'Article I.5':\n%s", name, out)
			}
		})
	}
}

// TestClusterRoleHasExactlyFourRules locks the FR-21 + R-9 + Article I.1
// invariant: the rendered ClusterRole carries the four specific verb
// sets and NOTHING ELSE. Runtime AC-12 covers `auth can-i` checks; this
// test is the build-time half.
func TestClusterRoleHasExactlyFourRules(t *testing.T) {
	out, err := helmTemplate(t, "--set", "clusterName=cf1z")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	// Expected verb sets — check each substring is present in the
	// ClusterRole section.
	required := []string{
		`resources: ["nodes"]
    verbs: ["get", "list", "watch", "patch"]`,
		`resources: ["pods"]
    verbs: ["get", "list", "watch"]`,
		`resources: ["pods/eviction"]
    verbs: ["create"]`,
		`resources: ["nodehealthdiagnosisais"]
    verbs: ["get", "list", "watch", "patch"]`,
	}
	for _, r := range required {
		if !strings.Contains(out, r) {
			t.Errorf("ClusterRole missing required rule:\n%s", r)
		}
	}

	// Forbidden verbs — Article I.1 floor. Drop the comment-block
	// which mentions these verbs by name in human-readable text;
	// only the rules-list section is the authoritative surface.
	body := dropCommentLines(out)
	forbidden := map[string]string{
		"nodes/delete": "Article I.1 forbids nodes/delete on the UI ClusterRole",
		"nodes/create": "Article I.1 forbids nodes/create on the UI ClusterRole",
		"secrets":      "Article I.1 forbids secrets on the UI ClusterRole",
		"configmaps":   "Article I.1 forbids configmaps on the UI ClusterRole",
		"/finalizers":  "Article I.1 forbids /finalizers subresource on the UI ClusterRole",
	}
	for substr, msg := range forbidden {
		if strings.Contains(body, substr) {
			t.Errorf("%s — found %q in rendered chart body", msg, substr)
		}
	}
	// Wildcard verb check — the literal `"*"` is forbidden inside any
	// `verbs:` list. Search line-by-line so an unrelated string match
	// (e.g., a label value) doesn't trip the test.
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "verbs:") && strings.Contains(trim, `"*"`) {
			t.Errorf("Article I.1 forbids wildcard verbs on the UI ClusterRole; found: %s", trim)
		}
	}
}

// TestDeploymentSecurityContext verifies plan §Constraints
// (runAsNonRoot, readOnlyRootFilesystem, drop ALL caps).
func TestDeploymentSecurityContext(t *testing.T) {
	out, err := helmTemplate(t, "--set", "clusterName=cf1z")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	required := []string{
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		`drop: ["ALL"]`,
	}
	for _, r := range required {
		if !strings.Contains(out, r) {
			t.Errorf("Deployment missing security-context invariant: %q", r)
		}
	}
}

// dropCommentLines returns out with `#`-prefixed YAML comment lines
// stripped. Used so the forbidden-verb checks scan only the live
// chart body, not the rationale comments inside clusterrole.yaml.
func dropCommentLines(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
