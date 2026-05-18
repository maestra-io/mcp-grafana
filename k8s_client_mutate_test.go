package mcpgrafana

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestKubernetesClient_Update asserts PUT method, escaped path, request body
// passthrough, and decoded response.
func TestKubernetesClient_Update(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"metadata": map[string]interface{}{"name": "abc", "resourceVersion": "99"},
		})
	}))
	defer ts.Close()

	c := &KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}
	obj := map[string]interface{}{"metadata": map[string]interface{}{"name": "abc", "resourceVersion": "98"}}
	res, err := c.Update(context.Background(), testDashboardDesc, "default", "abc", obj)
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if want := "/apis/dashboard.grafana.app/v2beta1/namespaces/default/dashboards/abc"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
	if !strings.Contains(gotBody, `"resourceVersion":"98"`) {
		t.Errorf("PUT body did not carry the sent object: %s", gotBody)
	}
	if md, _ := res["metadata"].(map[string]interface{}); md["name"] != "abc" {
		t.Errorf("unexpected result metadata: %v", res)
	}
}

// TestKubernetesClient_Create asserts POST method, collection path, request
// body passthrough, and decoded response.
func TestKubernetesClient_Create(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"metadata": map[string]interface{}{"name": "generated-uid"},
		})
	}))
	defer ts.Close()

	c := &KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}
	res, err := c.Create(context.Background(), testDashboardDesc, "default",
		map[string]interface{}{"kind": "Dashboard", "spec": map[string]interface{}{"title": "X"}})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if want := "/apis/dashboard.grafana.app/v2beta1/namespaces/default/dashboards"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
	if !strings.Contains(gotBody, `"kind":"Dashboard"`) {
		t.Errorf("POST body did not carry the sent object: %s", gotBody)
	}
	if md, _ := res["metadata"].(map[string]interface{}); md["name"] != "generated-uid" {
		t.Errorf("unexpected result metadata: %v", res)
	}
}

// TestKubernetesClient_Mutate_EmptyBody verifies a 2xx with an empty body is
// reported as success (empty map), not a decode error.
func TestKubernetesClient_Mutate_EmptyBody(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNoContent} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		c := &KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}
		res, err := c.Update(context.Background(), testDashboardDesc, "default", "abc", map[string]interface{}{})
		ts.Close()
		if err != nil {
			t.Fatalf("code %d: Update() error: %v", code, err)
		}
		if res == nil || len(res) != 0 {
			t.Errorf("code %d: result = %v, want empty map", code, res)
		}
	}
}

// TestKubernetesClient_Mutate_BadJSON verifies a 2xx with a non-JSON body
// surfaces a decode error tagged with the HTTP method.
func TestKubernetesClient_Mutate_BadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer ts.Close()
	c := &KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}
	_, err := c.Create(context.Background(), testDashboardDesc, "default", map[string]interface{}{})
	if err == nil || !strings.Contains(err.Error(), "decode POST response") {
		t.Errorf("error = %v, want a 'decode POST response' decode error", err)
	}
}

// TestKubernetesClient_Mutate_APIError verifies a non-2xx surfaces a
// *KubernetesAPIError discoverable via errors.As.
func TestKubernetesClient_Mutate_APIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"kind": "Status", "code": 409})
	}))
	defer ts.Close()
	c := &KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}
	_, err := c.Update(context.Background(), testDashboardDesc, "default", "abc", map[string]interface{}{})
	var apiErr *KubernetesAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Errorf("error = %v, want *KubernetesAPIError with 409", err)
	}
}

// TestValidatePathSegment covers the shared namespace/name guard used by
// Get/List/Update/Create.
func TestValidatePathSegment(t *testing.T) {
	bad := []string{"", ".", "..", "a/b", "a\\b", "a?b", "a#b", "a b", "a%2eb", "a@b", "a..b/c"}
	for _, v := range bad {
		if err := validatePathSegment("name", v); err == nil {
			t.Errorf("validatePathSegment(%q) = nil, want error", v)
		}
	}
	good := []string{"default", "stacks-12345", "org-1", "abc", "My-Dash_01", "a.b-c", "v2beta1"}
	for _, v := range good {
		if err := validatePathSegment("name", v); err != nil {
			t.Errorf("validatePathSegment(%q) = %v, want nil", v, err)
		}
	}
}

// TestKubernetesClient_Mutate_RejectsBadSegment verifies invalid name/namespace
// is rejected before any HTTP request is issued.
func TestKubernetesClient_Mutate_RejectsBadSegment(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer ts.Close()
	c := &KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}

	if _, err := c.Update(context.Background(), testDashboardDesc, "default", "../evil", map[string]interface{}{}); err == nil {
		t.Error("Update with traversal name: expected error")
	}
	if _, err := c.Update(context.Background(), testDashboardDesc, "default", "", map[string]interface{}{}); err == nil {
		t.Error("Update with empty name: expected error")
	}
	if _, err := c.Create(context.Background(), testDashboardDesc, "bad/ns", map[string]interface{}{}); err == nil {
		t.Error("Create with traversal namespace: expected error")
	}
	if called {
		t.Error("an HTTP request was issued despite invalid path segment")
	}
}
