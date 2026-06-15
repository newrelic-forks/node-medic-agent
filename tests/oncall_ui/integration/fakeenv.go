// Package integration carries httptest-driven coverage of the on-call
// UI's HTTP surface. The shared scaffolding (fake client builder,
// scheme) lives here so each test file stays focused on its own AC.
package integration

import (
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"net/http"

	"k8s.io/node-problem-detector/internal/oncall/handlers"
	"k8s.io/node-problem-detector/internal/oncall/render"
	"k8s.io/node-problem-detector/internal/oncall/server"
)

// buildScheme registers every API the UI reads or writes against a
// fresh runtime.Scheme. Mirrors internal/oncall/kube.BuildScheme but
// is local to tests so we don't depend on the runtime
// rest.Config-resolving entry point.
func buildScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("register corev1: %v", err)
	}
	if err := policyv1.AddToScheme(sch); err != nil {
		t.Fatalf("register policyv1: %v", err)
	}
	if err := nodemedicv1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("register nodemedic: %v", err)
	}
	return sch
}

// newFakeClient returns a controller-runtime fake client seeded with
// objs. UI-side patches against the NHD via client.MergeFrom hit the
// fake's in-memory store and survive across calls.
//
// The Pod spec.nodeName field index is registered so drain.PlanForNode
// can use the same field-selector shape it uses against the real
// apiserver (research R-3, FR-24).
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := buildScheme(t)
	return fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithIndex(&corev1.Pod{}, "spec.nodeName", podNodeNameIndexer).
		Build()
}

// newFakeClientWithFuncs is newFakeClient plus an interceptor — used
// by tests that simulate apiserver errors.
func newFakeClientWithFuncs(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	sch := buildScheme(t)
	return fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		WithIndex(&corev1.Pod{}, "spec.nodeName", podNodeNameIndexer).
		Build()
}

// podNodeNameIndexer mirrors the apiserver's spec.nodeName index so
// the fake client can serve fields.OneTermEqualSelector queries.
func podNodeNameIndexer(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	return []string{pod.Spec.NodeName}
}

// newTestServer builds a server.Server with the given client + Phase 4
// handlers attached + static asset handler attached. Phase 5 + 6 will
// extend this helper to attach the uncordon / clear-skip / drain
// handlers; Phase 4's tests only exercise list + detail.
func newTestServer(t *testing.T, c client.Client) *httptest.Server {
	t.Helper()
	srv := server.NewStubServer()
	listDeps := handlers.ListDeps{Client: c, Namespace: srv.Cfg().Namespace, Logger: srv.Logger()}
	detailDeps := handlers.DetailDeps{Client: c, Namespace: srv.Cfg().Namespace, Logger: srv.Logger()}
	srv.SetRouteHandler("GET /", handlers.ListHTML(listDeps))
	srv.SetRouteHandler("GET /api/cases", handlers.ListJSON(listDeps))
	srv.SetRouteHandler("GET /cases/{nhd}", handlers.DetailHTML(detailDeps))

	attachActionHandlers(srv, c)

	srv.SetStaticHandler(staticHandler())

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// attachActionHandlers wires the Phase 5 (US3) + Phase 6 (US4)
// action endpoints. Phase 5 ships uncordon + clear-skip-deletion;
// Phase 6 swaps in the real Drain handler.
func attachActionHandlers(srv *server.Server, c client.Client) {
	actDeps := handlers.ActionDeps{Client: c, Namespace: srv.Cfg().Namespace, Logger: srv.Logger(), AuditBufferSize: srv.Cfg().AuditBufferSize}
	srv.SetRouteHandler("POST /api/cases/{nhd}/actions/uncordon", handlers.Uncordon(actDeps))
	srv.SetRouteHandler("POST /api/cases/{nhd}/actions/clear-skip-deletion", handlers.ClearSkipDeletion(actDeps))
	drainDeps := handlers.DrainDeps{Client: c, Namespace: srv.Cfg().Namespace, Logger: srv.Logger(), DrainConcurrency: srv.Cfg().DrainConcurrency, AuditBufferSize: srv.Cfg().AuditBufferSize}
	srv.SetRouteHandler("POST /api/cases/{nhd}/actions/drain", handlers.Drain(drainDeps))
}

// staticHandler builds the same /static/ handler main.go does.
func staticHandler() http.Handler {
	return http.StripPrefix("/static/", http.FileServer(http.FS(render.StaticFS())))
}
