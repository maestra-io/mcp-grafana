package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpgrafana "github.com/grafana/mcp-grafana"
)

func TestKubernetesDashboardInfo(t *testing.T) {
	cases := []struct {
		name    string
		dash    map[string]interface{}
		wantVer string
		wantOK  bool
	}{
		{"v2beta1", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v2beta1"}, "v2beta1", true},
		{"v2alpha1", map[string]interface{}{"apiVersion": "dashboard.grafana.app/v2alpha1"}, "v2alpha1", true},
		{"legacy no apiVersion", map[string]interface{}{"title": "x", "panels": []interface{}{}}, "", false},
		{"other group", map[string]interface{}{"apiVersion": "folder.grafana.app/v1beta1"}, "", false},
		{"non-string apiVersion", map[string]interface{}{"apiVersion": 1}, "", false},
		{"empty", map[string]interface{}{}, "", false},
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

func TestDashboardNamespace(t *testing.T) {
	t.Setenv("GRAFANA_DASHBOARD_NAMESPACE", "")
	if ns := dashboardNamespace(); ns != "default" {
		t.Errorf("dashboardNamespace() default = %q, want %q", ns, "default")
	}
	t.Setenv("GRAFANA_DASHBOARD_NAMESPACE", "stacks-42")
	if ns := dashboardNamespace(); ns != "stacks-42" {
		t.Errorf("dashboardNamespace() override = %q, want %q", ns, "stacks-42")
	}
}

// v2 create path: GET probe 404 -> POST to the collection.
func TestUpdateDashboardV2_Create(t *testing.T) {
	var methods, paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"kind": "Status", "code": 404})
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"metadata": map[string]interface{}{"name": "new-uid"},
			})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer ts.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{URL: ts.URL})
	body, err := updateDashboardWithFullJSON(ctx, UpdateDashboardParams{
		Dashboard: map[string]interface{}{
			"apiVersion": "dashboard.grafana.app/v2beta1",
			"kind":       "Dashboard",
			"metadata":   map[string]interface{}{"name": "new-uid"},
			"spec": map[string]interface{}{
				"title":  "X",
				"layout": map[string]interface{}{"kind": "AutoGridLayout"},
			},
		},
		FolderUID: "fold1",
	})
	if err != nil {
		t.Fatalf("updateDashboardWithFullJSON() error: %v", err)
	}
	if body.UID == nil || *body.UID != "new-uid" {
		t.Errorf("UID = %v, want new-uid", body.UID)
	}
	if body.Status == nil || *body.Status != "success" {
		t.Errorf("Status = %v, want success", body.Status)
	}
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodPost {
		t.Errorf("methods = %v, want [GET POST]", methods)
	}
	wantBase := "/apis/dashboard.grafana.app/v2beta1/namespaces/default/dashboards"
	if paths[1] != wantBase {
		t.Errorf("POST path = %s, want %s", paths[1], wantBase)
	}
}

// v2 update path: GET returns existing -> resourceVersion injected into PUT body.
func TestUpdateDashboardV2_Update(t *testing.T) {
	var sawPUT bool
	var putBody map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"metadata": map[string]interface{}{"name": "abc", "resourceVersion": "777"},
			})
		case http.MethodPut:
			sawPUT = true
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &putBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"metadata": map[string]interface{}{"name": "abc"},
			})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer ts.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{URL: ts.URL})
	_, err := updateDashboardWithFullJSON(ctx, UpdateDashboardParams{
		Dashboard: map[string]interface{}{
			"apiVersion": "dashboard.grafana.app/v2beta1",
			"kind":       "Dashboard",
			"metadata":   map[string]interface{}{"name": "abc"},
			"spec":       map[string]interface{}{"title": "X"},
		},
	})
	if err != nil {
		t.Fatalf("updateDashboardWithFullJSON() error: %v", err)
	}
	if !sawPUT {
		t.Fatal("expected a PUT request for an existing v2 dashboard")
	}
	md, _ := putBody["metadata"].(map[string]interface{})
	if md["resourceVersion"] != "777" {
		t.Errorf("PUT body resourceVersion = %v, want 777 (fetched from GET for optimistic concurrency)", md["resourceVersion"])
	}
	ann, _ := md["annotations"].(map[string]interface{})
	_ = ann // folder annotation only set when FolderUID provided; not asserted here
}

// Routing decision for legacy (v1) dashboards is asserted by
// TestKubernetesDashboardInfo: a v1 payload yields ok=false, so
// updateDashboardWithFullJSON never enters the apiserver path and falls
// through to the unchanged legacy save endpoint.
