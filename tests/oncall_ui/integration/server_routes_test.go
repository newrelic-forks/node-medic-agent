// Package integration carries httptest-driven coverage of the on-call
// UI's HTTP surface.
package integration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/node-problem-detector/internal/oncall/server"
)

// TestRouteTable locks the on-the-wire shape of the six UI routes:
// the path/method pairs the OpenAPI contract names, and the
// Content-Type each handler MUST set even while the body is a
// 501-stub.
//
// Phase 2 stubs return 501 Not Implemented; later phases (US2/US3/US4)
// fill in the real bodies. The route table itself doesn't change after
// this test — every later phase should keep this test green.
func TestRouteTable(t *testing.T) {
	srv := server.NewStubServer()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cases := []struct {
		name         string
		method       string
		path         string
		wantStatus   int   // 501 today; later phases relax this for real handlers
		acceptStatus []int // alternative accepted statuses (e.g. 200 once real impl lands)
		wantContent  string
	}{
		{
			name:         "GET / list page",
			method:       http.MethodGet,
			path:         "/",
			wantStatus:   http.StatusNotImplemented,
			acceptStatus: []int{http.StatusOK},
			wantContent:  "text/html",
		},
		{
			name:         "GET /api/cases JSON",
			method:       http.MethodGet,
			path:         "/api/cases",
			wantStatus:   http.StatusNotImplemented,
			acceptStatus: []int{http.StatusOK},
			wantContent:  "application/json",
		},
		{
			name:         "GET /cases/{nhd} detail page",
			method:       http.MethodGet,
			path:         "/cases/example-nhd",
			wantStatus:   http.StatusNotImplemented,
			acceptStatus: []int{http.StatusOK, http.StatusNotFound},
			wantContent:  "text/html",
		},
		{
			name:         "POST uncordon",
			method:       http.MethodPost,
			path:         "/api/cases/example-nhd/actions/uncordon",
			wantStatus:   http.StatusNotImplemented,
			acceptStatus: []int{http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict},
			wantContent:  "application/json",
		},
		{
			name:         "POST clear-skip-deletion",
			method:       http.MethodPost,
			path:         "/api/cases/example-nhd/actions/clear-skip-deletion",
			wantStatus:   http.StatusNotImplemented,
			acceptStatus: []int{http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict},
			wantContent:  "application/json",
		},
		{
			name:         "POST drain SSE",
			method:       http.MethodPost,
			path:         "/api/cases/example-nhd/actions/drain",
			wantStatus:   http.StatusNotImplemented,
			acceptStatus: []int{http.StatusOK, http.StatusConflict, http.StatusBadRequest, http.StatusNotFound},
			wantContent:  "text/event-stream",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()

			ok := resp.StatusCode == tc.wantStatus
			for _, s := range tc.acceptStatus {
				if resp.StatusCode == s {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s %s: status = %d, want %d (or %v)",
					tc.method, tc.path, resp.StatusCode, tc.wantStatus, tc.acceptStatus)
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.Contains(ct, tc.wantContent) {
				t.Errorf("%s %s: Content-Type = %q, want substring %q",
					tc.method, tc.path, ct, tc.wantContent)
			}
		})
	}
}

// TestActionEndpointsHaveNoStoreCacheControl verifies FR-spec'd
// middleware behavior: every action endpoint sets Cache-Control:
// no-store so a browser back-button doesn't replay a mutation. The
// list and detail pages do NOT need this header (they're idempotent
// reads).
func TestActionEndpointsHaveNoStoreCacheControl(t *testing.T) {
	srv := server.NewStubServer()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	actions := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/cases/foo/actions/uncordon"},
		{http.MethodPost, "/api/cases/foo/actions/clear-skip-deletion"},
		{http.MethodPost, "/api/cases/foo/actions/drain"},
	}
	for _, a := range actions {
		t.Run(a.method+" "+a.path, func(t *testing.T) {
			req, err := http.NewRequest(a.method, ts.URL+a.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
				t.Errorf("%s %s: Cache-Control = %q, want substring %q", a.method, a.path, got, "no-store")
			}
		})
	}
}
