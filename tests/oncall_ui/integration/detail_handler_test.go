// T039 — detail handler integration test. Each fixture exercises a
// distinct branch in ComposeDetailPageData / DetailHTML.
package integration

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDetailHTML_AppliedHappyPath(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_applied.yaml")
	node := nodeFor(nhd.Spec.Case.NodeName, true /* unschedulable */, true /* skipDeletion */)
	c := newFakeClient(t, nhd, node)
	ts := newTestServer(t, c)

	resp, err := ts.Client().Get(ts.URL + "/cases/" + nhd.Name)
	if err != nil {
		t.Fatalf("GET /cases/X: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)

	// Section content + button enablement state
	for _, want := range []string{
		nhd.Spec.Case.NodeName,
		"Action history",
		"Cordon",
		"controller",
		"Uncordon Node",
		"Clear MLC skipDeletion",
		"Drain Node",
		"Agent diagnosis",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("detail HTML missing %q", want)
		}
	}
	// Buttons should NOT be disabled.
	for _, badly := range []string{
		`data-action="uncordon"
              data-nhd="`, // any disabled marker would be inside this same tag
	} {
		_ = badly
	}
	if strings.Contains(s, `data-action="uncordon"`) && strings.Contains(s, `disabled title="Already uncordoned"`) {
		t.Errorf("uncordon button disabled but node is cordoned (fixture)")
	}
}

func TestDetailHTML_HumanInLoop(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_human_in_loop.yaml")
	node := nodeFor(nhd.Spec.Case.NodeName, false /* uncordoned */, false /* no skipDeletion */)
	c := newFakeClient(t, nhd, node)
	ts := newTestServer(t, c)

	resp, _ := ts.Client().Get(ts.URL + "/cases/" + nhd.Name)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `disabled title="Already uncordoned"`) {
		t.Errorf("uncordon button must be disabled (node not cordoned)")
	}
	if !strings.Contains(s, `disabled title="Annotation already cleared"`) {
		t.Errorf("clear-skip button must be disabled (no annotation)")
	}
}

func TestDetailHTML_DiagnosingShowsBanner(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_pending_diagnosing.yaml")
	node := nodeFor(nhd.Spec.Case.NodeName, true, true)
	c := newFakeClient(t, nhd, node)
	ts := newTestServer(t, c)

	resp, _ := ts.Client().Get(ts.URL + "/cases/" + nhd.Name)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "Diagnosis in progress") {
		t.Errorf("phase banner missing (FR-9a)")
	}
	// FR-9a — buttons stay enabled per node state, NOT phase. Node
	// is cordoned + skipDeletion-stamped so both button-disabled
	// markers must be absent.
	if strings.Contains(s, `disabled title="Already uncordoned"`) {
		t.Errorf("uncordon button must NOT be disabled during Diagnosing if node is cordoned (FR-9a)")
	}
	if strings.Contains(s, `disabled title="Annotation already cleared"`) {
		t.Errorf("clear-skip button must NOT be disabled during Diagnosing if annotation is present (FR-9a)")
	}
}

func TestDetailHTML_ReclaimedNode(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_node_reclaimed.yaml")
	// Deliberately do NOT seed the Node CR — the fake client returns
	// NotFound, which DetailHTML interprets as the FR-17a reclaimed
	// branch.
	c := newFakeClient(t, nhd)
	ts := newTestServer(t, c)

	resp, _ := ts.Client().Get(ts.URL + "/cases/" + nhd.Name)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "Node was reclaimed by MLC") {
		t.Errorf("reclaimed banner missing")
	}
	for _, want := range []string{
		`disabled title="Node was reclaimed by MLC"`,
	} {
		if strings.Count(s, want) < 3 {
			t.Errorf("expected all 3 buttons disabled with %q tooltip; saw %d", want, strings.Count(s, want))
		}
	}
}

func TestDetailHTML_MissingNHD(t *testing.T) {
	c := newFakeClient(t)
	ts := newTestServer(t, c)

	resp, err := ts.Client().Get(ts.URL + "/cases/no-such-nhd")
	if err != nil {
		t.Fatalf("GET /cases/no-such-nhd: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (FR-12 — page exists, entity doesn't)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Case not found") {
		t.Errorf("case-not-found panel missing")
	}
}

func TestDetailHTML_ApiserverErrorBanner(t *testing.T) {
	failingFuncs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
			return errors.New("simulated outage")
		},
	}
	cl := newFakeClientWithFuncs(t, failingFuncs)
	ts := newTestServer(t, cl)

	resp, err := ts.Client().Get(ts.URL + "/cases/some-nhd")
	if err != nil {
		t.Fatalf("GET /cases/some-nhd: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (FR-13)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "apiserver unreachable") {
		t.Errorf("error banner missing")
	}
}
