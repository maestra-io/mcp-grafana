package mcpgrafana

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKubernetesClient_Update(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"kind":       "Dashboard",
			"apiVersion": "dashboard.grafana.app/v2beta1",
			"metadata":   map[string]interface{}{"name": "abc", "resourceVersion": "99"},
		})
	}))
	defer ts.Close()

	c := &KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}
	obj := map[string]interface{}{
		"apiVersion": "dashboard.grafana.app/v2beta1",
		"kind":       "Dashboard",
		"metadata":   map[string]interface{}{"name": "abc", "resourceVersion": "98"},
	}
	res, err := c.Update(context.Background(), testDashboardDesc, "default", "abc", obj)
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	wantPath := "/apis/dashboard.grafana.app/v2beta1/namespaces/default/dashboards/abc"
	if gotPath != wantPath {
		t.Errorf("path = %s, want %s", gotPath, wantPath)
	}
	if !strings.Contains(gotBody, `"resourceVersion":"98"`) {
		t.Errorf("PUT body did not carry the sent object: %s", gotBody)
	}
	if md, _ := res["metadata"].(map[string]interface{}); md["name"] != "abc" {
		t.Errorf("unexpected result metadata: %v", res)
	}
}

func TestKubernetesClient_Create(t *testing.T) {
	var gotMethod, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"metadata": map[string]interface{}{"name": "generated-uid"},
		})
	}))
	defer ts.Close()

	c := &KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}
	res, err := c.Create(context.Background(), testDashboardDesc, "default", map[string]interface{}{"kind": "Dashboard"})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	wantPath := "/apis/dashboard.grafana.app/v2beta1/namespaces/default/dashboards"
	if gotPath != wantPath {
		t.Errorf("path = %s, want %s", gotPath, wantPath)
	}
	if md, _ := res["metadata"].(map[string]interface{}); md["name"] != "generated-uid" {
		t.Errorf("unexpected result metadata: %v", res)
	}
}

func TestKubernetesClient_Update_RejectsPathTraversal(t *testing.T) {
	c := &KubernetesClient{BaseURL: "http://example.invalid", HTTPClient: http.DefaultClient}
	if _, err := c.Update(context.Background(), testDashboardDesc, "default", "../evil", map[string]interface{}{}); err == nil {
		t.Error("expected error for name containing a path separator")
	}
	if _, err := c.Create(context.Background(), testDashboardDesc, "bad/ns", map[string]interface{}{}); err == nil {
		t.Error("expected error for namespace containing a path separator")
	}
}

func TestKubernetesClient_Update_APIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"kind": "Status", "status": "Failure", "code": 409,
			"message": "the object has been modified",
		})
	}))
	defer ts.Close()

	c := &KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}
	_, err := c.Update(context.Background(), testDashboardDesc, "default", "abc", map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error on 409 conflict")
	}
	var apiErr *KubernetesAPIError
	if !asKubernetesAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Errorf("expected KubernetesAPIError with 409, got %v", err)
	}
}

// asKubernetesAPIError unwraps err looking for a *KubernetesAPIError.
func asKubernetesAPIError(err error, target **KubernetesAPIError) bool {
	for err != nil {
		if e, ok := err.(*KubernetesAPIError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
