package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpgrafana "github.com/grafana/mcp-grafana"
)

// TestKubernetesDashboardInfo covers v2 detection and version-segment
// validation (rejects empty / multi-segment / malformed / non-string).
func TestKubernetesDashboardInfo(t *testing.T) {
	cases := []struct {
		name    string
		dash    map[string]interface{}
		wantVer string
		wantOK  bool
	}{
		{"v1", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v1"}, "v1", true},
		{"v2beta1", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v2beta1"}, "v2beta1", true},
		{"v2alpha1", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v2alpha1"}, "v2alpha1", true},
		{"v22beta3", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v22beta3"}, "v22beta3", true},
		{"legacy no apiVersion", map[string]interface{}{"title": "x", "panels": []interface{}{}}, "", false},
		{"other group", map[string]interface{}{"apiVersion": "folder.grafana.app/v1beta1"}, "", false},
		{"non-string apiVersion", map[string]interface{}{"apiVersion": 1}, "", false},
		{"empty", map[string]interface{}{}, "", false},
		{"empty version suffix", map[string]interface{}{"apiVersion": "dashboard.grafana.app/"}, "", false},
		{"multi-segment suffix", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v2beta1/x"}, "", false},
		{"backslash in suffix", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v2\\bad"}, "", false},
		{"query injection", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v1?watch=true"}, "", false},
		{"no digits", map[string]interface{}{"apiVersion": "dashboard.grafana.app/vbeta1"}, "", false},
		{"trailing beta no digits", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v2beta"}, "", false},
		{"unknown channel", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v2gamma1"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ver, ok := kubernetesDashboardInfo(tc.dash)
			if ok != tc.wantOK || ver != tc.wantVer {
				t.Errorf("kubernetesDashboardInfo() = (%q, %v), want (%q, %v)", ver, ok, tc.wantVer, tc.wantOK)
			}
		})
	}
}

// TestDashboardNamespace covers the default and the env-var override.
func TestDashboardNamespace(t *testing.T) {
	t.Setenv("GRAFANA_DASHBOARD_NAMESPACE", "")
	if ns := dashboardNamespace(); ns != "default" {
		t.Errorf("default = %q, want default", ns)
	}
	t.Setenv("GRAFANA_DASHBOARD_NAMESPACE", "stacks-42")
	if ns := dashboardNamespace(); ns != "stacks-42" {
		t.Errorf("override = %q, want stacks-42", ns)
	}
}

// TestIsKubernetesNotFound covers the 404-only create predicate, including
// wrapped errors and non-404 API errors.
func TestIsKubernetesNotFound(t *testing.T) {
	notFound := &mcpgrafana.KubernetesAPIError{StatusCode: http.StatusNotFound}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic", fmt.Errorf("boom"), false},
		{"wrapped non-api", fmt.Errorf("ctx: %w", context.Canceled), false},
		{"direct 404", notFound, true},
		{"wrapped 404", fmt.Errorf("probe: %w", notFound), true},
		{"403", &mcpgrafana.KubernetesAPIError{StatusCode: http.StatusForbidden}, false},
		{"500", &mcpgrafana.KubernetesAPIError{StatusCode: http.StatusInternalServerError}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isKubernetesNotFound(tc.err); got != tc.want {
				t.Errorf("isKubernetesNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// v2crServer is a test apiserver. get returns (status, body); put/post capture
// the request body and return (status, body).
type v2crServer struct {
	getStatus       int
	getBody         map[string]interface{}
	writeStatus     int
	writeBody       map[string]interface{}
	gotMethods      []string
	lastWriteBody   map[string]interface{}
	lastWriteMethod string
}

// handler returns an httptest handler: GET serves getStatus/getBody; PUT and
// POST capture the request body and reply writeStatus (default 200)/writeBody.
func (s *v2crServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		s.gotMethods = append(s.gotMethods, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(s.getStatus)
			_ = json.NewEncoder(w).Encode(s.getBody)
		case http.MethodPut, http.MethodPost:
			s.lastWriteMethod = r.Method
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &s.lastWriteBody)
			st := s.writeStatus
			if st == 0 {
				st = http.StatusOK
			}
			w.WriteHeader(st)
			_ = json.NewEncoder(w).Encode(s.writeBody)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}
}

// v2ctx returns a context carrying a GrafanaConfig pointed at url.
func v2ctx(url string) context.Context {
	return mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{URL: url})
}

// v2dash builds a minimal v2 dashboard map; a non-empty name sets
// metadata.name.
func v2dash(name string) map[string]interface{} {
	m := map[string]interface{}{
		"apiVersion": "dashboard.grafana.app/v2beta1",
		"kind":       "Dashboard",
		"spec":       map[string]interface{}{"title": "X"},
	}
	if name != "" {
		m["metadata"] = map[string]interface{}{"name": name}
	}
	return m
}

// TestUpdateDashboardV2_Create: existing-name probe 404 -> POST; folder
// annotation written; server-managed metadata stripped.
func TestUpdateDashboardV2_Create(t *testing.T) {
	srv := &v2crServer{getStatus: http.StatusNotFound, getBody: map[string]interface{}{"kind": "Status", "code": 404},
		writeBody: map[string]interface{}{"metadata": map[string]interface{}{"name": "new-uid"}}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	d := v2dash("new-uid")
	d["metadata"].(map[string]interface{})["resourceVersion"] = "stale-99" // must be stripped on create
	body, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: d, FolderUID: "fold1"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if body.UID == nil || *body.UID != "new-uid" || body.Status == nil || *body.Status != "success" {
		t.Errorf("body = %+v, want UID=new-uid Status=success", body)
	}
	if len(srv.gotMethods) != 2 || srv.gotMethods[0] != "GET" || srv.gotMethods[1] != "POST" {
		t.Errorf("methods = %v, want [GET POST]", srv.gotMethods)
	}
	meta, _ := srv.lastWriteBody["metadata"].(map[string]interface{})
	if _, present := meta["resourceVersion"]; present {
		t.Error("POST body still carries server-managed resourceVersion")
	}
	ann, _ := meta["annotations"].(map[string]interface{})
	if ann["grafana.app/folder"] != "fold1" {
		t.Errorf("folder annotation = %v, want fold1", ann["grafana.app/folder"])
	}
}

// TestUpdateDashboardV2_Update: probe 200 + overwrite -> PUT that mirrors
// saveDashboardViaK8s — server-managed metadata stripped (incl. uid and
// resourceVersion: an unconditional replace, no TOCTOU 409), name forced,
// folder annotation written.
func TestUpdateDashboardV2_Update(t *testing.T) {
	srv := &v2crServer{getStatus: http.StatusOK,
		getBody:   map[string]interface{}{"metadata": map[string]interface{}{"name": "abc", "resourceVersion": "777"}},
		writeBody: map[string]interface{}{"metadata": map[string]interface{}{"name": "abc"}}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	d := v2dash("abc")
	dm := d["metadata"].(map[string]interface{})
	dm["namespace"] = "should-be-dropped"
	dm["managedFields"] = []interface{}{"x"}
	dm["uid"] = "stale-foreign-uid"
	dm["resourceVersion"] = "stale-1"
	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: d, FolderUID: "f2", Overwrite: true})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if srv.lastWriteMethod != "PUT" {
		t.Fatalf("write method = %s, want PUT", srv.lastWriteMethod)
	}
	meta, _ := srv.lastWriteBody["metadata"].(map[string]interface{})
	if meta["name"] != "abc" {
		t.Errorf("PUT metadata.name = %v, want abc", meta["name"])
	}
	for _, k := range []string{"resourceVersion", "uid", "namespace", "managedFields"} {
		if _, ok := meta[k]; ok {
			t.Errorf("PUT body still carries server-managed metadata.%s", k)
		}
	}
	if ann, _ := meta["annotations"].(map[string]interface{}); ann["grafana.app/folder"] != "f2" {
		t.Errorf("folder annotation = %v, want f2", meta["annotations"])
	}
}

// TestUpdateDashboardV2_OverwriteFalse: existing dashboard + overwrite=false
// must error and must NOT issue a write.
func TestUpdateDashboardV2_OverwriteFalse(t *testing.T) {
	srv := &v2crServer{getStatus: http.StatusOK,
		getBody: map[string]interface{}{"metadata": map[string]interface{}{"name": "abc", "resourceVersion": "1"}}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: v2dash("abc")})
	if err == nil {
		t.Fatal("expected error when dashboard exists and overwrite=false")
	}
	if srv.lastWriteMethod != "" {
		t.Errorf("a %s write was issued despite overwrite=false", srv.lastWriteMethod)
	}
}

// TestUpdateDashboardV2_NoName_DirectCreate: no metadata.name and no args.UID
// -> straight POST, no GET probe.
func TestUpdateDashboardV2_NoName_DirectCreate(t *testing.T) {
	srv := &v2crServer{writeBody: map[string]interface{}{"metadata": map[string]interface{}{"name": "gen-1"}}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	body, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: v2dash("")})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(srv.gotMethods) != 1 || srv.gotMethods[0] != "POST" {
		t.Errorf("methods = %v, want [POST] (no GET probe)", srv.gotMethods)
	}
	if body.UID == nil || *body.UID != "gen-1" {
		t.Errorf("UID = %v, want gen-1 (from apiserver response)", body.UID)
	}
}

// TestUpdateDashboardV2_NoName_UsesArgsUID: metadata.name absent but args.UID
// supplied (patch-mode shape) -> probe uses that UID.
func TestUpdateDashboardV2_NoName_UsesArgsUID(t *testing.T) {
	var probePath string
	srv := &v2crServer{getStatus: http.StatusOK,
		getBody:   map[string]interface{}{"metadata": map[string]interface{}{"name": "uid-x", "resourceVersion": "5"}},
		writeBody: map[string]interface{}{"metadata": map[string]interface{}{"name": "uid-x"}}}
	base := srv.handler(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			probePath = r.URL.Path
		}
		base(w, r)
	}))
	defer ts.Close()

	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: v2dash(""), UID: "uid-x", Overwrite: true})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if want := "/apis/dashboard.grafana.app/v2beta1/namespaces/default/dashboards/uid-x"; probePath != want {
		t.Errorf("probe path = %s, want %s", probePath, want)
	}
}

// TestUpdateDashboardV2_ProbeError_NoCreate: a non-404 probe failure surfaces
// as an error and must NOT fall back to create.
func TestUpdateDashboardV2_ProbeError_NoCreate(t *testing.T) {
	srv := &v2crServer{getStatus: http.StatusInternalServerError, getBody: map[string]interface{}{"code": 500}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: v2dash("abc"), Overwrite: true})
	if err == nil {
		t.Fatal("expected error when probe returns 500")
	}
	if srv.lastWriteMethod != "" {
		t.Errorf("a %s write was issued after a non-404 probe failure", srv.lastWriteMethod)
	}
}

// TestUpdateDashboardV2_PutError: PUT failure is wrapped with the update
// context.
func TestUpdateDashboardV2_PutError(t *testing.T) {
	srv := &v2crServer{getStatus: http.StatusOK,
		getBody:     map[string]interface{}{"metadata": map[string]interface{}{"name": "abc", "resourceVersion": "1"}},
		writeStatus: http.StatusConflict, writeBody: map[string]interface{}{"code": 409}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: v2dash("abc"), Overwrite: true})
	if err == nil || !strings.Contains(err.Error(), "update existing v2 dashboard") {
		t.Errorf("error = %v, want it to wrap 'update existing v2 dashboard'", err)
	}
}

// TestUpdateDashboardV2_CreateError: POST failure is wrapped with the create
// context.
func TestUpdateDashboardV2_CreateError(t *testing.T) {
	srv := &v2crServer{getStatus: http.StatusNotFound, getBody: map[string]interface{}{"code": 404},
		writeStatus: http.StatusConflict, writeBody: map[string]interface{}{"code": 409}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: v2dash("abc")})
	if err == nil || !strings.Contains(err.Error(), "create v2 dashboard") {
		t.Errorf("error = %v, want it to wrap 'create v2 dashboard'", err)
	}
}

// TestUpdateDashboardV2_ResultNoMetadataName: when the write response carries
// no metadata.name the returned UID falls back to the request name.
func TestUpdateDashboardV2_ResultNoMetadataName(t *testing.T) {
	srv := &v2crServer{getStatus: http.StatusOK,
		getBody:   map[string]interface{}{"metadata": map[string]interface{}{"name": "abc", "resourceVersion": "1"}},
		writeBody: map[string]interface{}{}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	body, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: v2dash("abc"), Overwrite: true})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if body.UID == nil || *body.UID != "abc" {
		t.Errorf("UID = %v, want abc (request-name fallback)", body.UID)
	}
}

// TestUpdateDashboardV2_BadMetadataType: metadata of a non-object type fails
// loudly before any HTTP request.
func TestUpdateDashboardV2_BadMetadataType(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer ts.Close()

	d := map[string]interface{}{"apiVersion": "dashboard.grafana.app/v2beta1", "kind": "Dashboard", "metadata": "oops"}
	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: d})
	if err == nil {
		t.Fatal("expected error for non-object metadata")
	}
	if called {
		t.Error("an HTTP request was issued despite malformed metadata")
	}
}

// TestUpdateDashboardV2_DoesNotMutateCallerArg: the caller's dashboard map
// (and its metadata sub-map) must not be mutated by the write path.
func TestUpdateDashboardV2_DoesNotMutateCallerArg(t *testing.T) {
	srv := &v2crServer{getStatus: http.StatusOK,
		getBody:   map[string]interface{}{"metadata": map[string]interface{}{"name": "abc", "resourceVersion": "9"}},
		writeBody: map[string]interface{}{"metadata": map[string]interface{}{"name": "abc"}}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	d := v2dash("abc")
	callerMeta := d["metadata"].(map[string]interface{})
	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: d, FolderUID: "f", Overwrite: true})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if _, ok := callerMeta["resourceVersion"]; ok {
		t.Error("caller metadata was mutated: resourceVersion leaked back")
	}
	if _, ok := callerMeta["annotations"]; ok {
		t.Error("caller metadata was mutated: annotations leaked back")
	}
}

// TestUpdateDashboardV2_BadAnnotationsType: metadata.annotations of a
// non-object type fails loudly before any HTTP request.
func TestUpdateDashboardV2_BadAnnotationsType(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer ts.Close()

	d := map[string]interface{}{
		"apiVersion": "dashboard.grafana.app/v2beta1", "kind": "Dashboard",
		"metadata": map[string]interface{}{"name": "abc", "annotations": "oops"},
	}
	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: d, Overwrite: true})
	if err == nil || !strings.Contains(err.Error(), "metadata.annotations is not an object") {
		t.Errorf("error = %v, want a metadata.annotations type error", err)
	}
	if called {
		t.Error("an HTTP request was issued despite malformed annotations")
	}
}

// TestUpdateDashboardV2_PreservesExistingAnnotations: an existing annotation
// is kept alongside the injected folder annotation, and the caller's
// annotations map is not mutated.
func TestUpdateDashboardV2_PreservesExistingAnnotations(t *testing.T) {
	srv := &v2crServer{getStatus: http.StatusOK,
		getBody:   map[string]interface{}{"metadata": map[string]interface{}{"name": "abc", "resourceVersion": "1"}},
		writeBody: map[string]interface{}{"metadata": map[string]interface{}{"name": "abc"}}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	d := v2dash("abc")
	callerAnn := map[string]interface{}{"foo": "bar"}
	d["metadata"].(map[string]interface{})["annotations"] = callerAnn
	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: d, FolderUID: "f9", Overwrite: true})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	meta, _ := srv.lastWriteBody["metadata"].(map[string]interface{})
	ann, _ := meta["annotations"].(map[string]interface{})
	if ann["foo"] != "bar" || ann["grafana.app/folder"] != "f9" {
		t.Errorf("PUT annotations = %v, want foo=bar and grafana.app/folder=f9", ann)
	}
	if _, leaked := callerAnn["grafana.app/folder"]; leaked {
		t.Error("caller annotations map was mutated: folder annotation leaked back")
	}
}

// TestUpdateDashboardV2_EmptyUIDGuard: a generated-name create whose response
// carries no metadata.name surfaces an explicit error rather than a bogus
// empty UID.
func TestUpdateDashboardV2_EmptyUIDGuard(t *testing.T) {
	srv := &v2crServer{writeBody: map[string]interface{}{}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	_, err := updateDashboardWithFullJSON(v2ctx(ts.URL), UpdateDashboardParams{Dashboard: v2dash("")})
	if err == nil || !strings.Contains(err.Error(), "carried no metadata.name") {
		t.Errorf("error = %v, want 'carried no metadata.name' guard", err)
	}
}
