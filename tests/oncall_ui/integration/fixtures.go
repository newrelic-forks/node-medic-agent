package integration

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// loadNHDFixture decodes one tests/oncall_ui/fixtures/*.yaml file into
// a typed NHD CR.
func loadNHDFixture(t *testing.T, name string) *nodemedicv1alpha1.NodeHealthDiagnosisAI {
	t.Helper()
	path := fixturePath(t, name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	nhd := &nodemedicv1alpha1.NodeHealthDiagnosisAI{}
	if err := yaml.Unmarshal(b, nhd); err != nil {
		t.Fatalf("decode fixture %s: %v", path, err)
	}
	return nhd
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "fixtures")
	return filepath.Join(root, name)
}

// nodeFor returns a corev1.Node CR seeded with the cordoned +
// skip-deletion state for the named NHD's spec.case.nodeName.
func nodeFor(name string, unschedulable, skipDeletion bool) *corev1.Node {
	n := &corev1.Node{}
	n.Name = name
	n.Spec.Unschedulable = unschedulable
	if skipDeletion {
		n.Annotations = map[string]string{"machine-lifecycle.newrelic.com/skipDeletion": "true"}
	}
	return n
}
