package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpgrafana "github.com/grafana/mcp-grafana/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// k8sDashboardObject is the full apiserver object form the fork's
// update_dashboard accepted (and get_dashboard_by_uid can return).
func k8sDashboardObject(spec map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "dashboard.grafana.app/v2beta1",
		"kind":       "Dashboard",
		"metadata": map[string]interface{}{
			"name":            "my-dash",
			"resourceVersion": "42",
			"annotations":     map[string]interface{}{"grafana.app/folder": "folder-a"},
		},
		"spec": spec,
	}
}

func TestUnwrapKubernetesDashboardObject(t *testing.T) {
	spec := map[string]interface{}{"title": "t", "elements": map[string]interface{}{}}

	t.Run("object form is unwrapped", func(t *testing.T) {
		in := k8sDashboardObject(spec)
		got := unwrapKubernetesDashboardObject(UpdateDashboardParams{Dashboard: in, Overwrite: true})
		assert.Equal(t, "my-dash", got.UID)
		assert.Equal(t, "folder-a", got.FolderUID)
		assert.True(t, got.Overwrite)
		assert.Equal(t, "t", got.Dashboard["title"])
		assert.Equal(t, "my-dash", got.Dashboard["uid"])
		assert.NotContains(t, got.Dashboard, "apiVersion")
		assert.NotContains(t, spec, "uid", "caller's spec must not be mutated")
		assert.True(t, isV2DashboardJSON(got.Dashboard))
	})

	t.Run("explicit args win", func(t *testing.T) {
		got := unwrapKubernetesDashboardObject(UpdateDashboardParams{Dashboard: k8sDashboardObject(spec), UID: "other", FolderUID: "folder-b"})
		assert.Equal(t, "other", got.UID)
		assert.Equal(t, "folder-b", got.FolderUID)
	})

	t.Run("plain bodies are untouched", func(t *testing.T) {
		for _, d := range []map[string]interface{}{
			{"title": "v1", "panels": []interface{}{}},
			{"title": "v2", "elements": map[string]interface{}{}},
			{"apiVersion": "dashboard.grafana.app/v2beta1"},                  // no spec
			{"apiVersion": "other.app/v1", "spec": map[string]interface{}{}}, // other group
		} {
			got := unwrapKubernetesDashboardObject(UpdateDashboardParams{Dashboard: d})
			assert.Equal(t, d, got.Dashboard)
			assert.Empty(t, got.UID)
		}
	})
}

// TestUpdateDashboard_KubernetesObjectFormCreatesV2 drives the fork's object
// form through updateDashboard and checks it reaches the apiserver as a v2
// create carrying the name and folder from the object's metadata.
func TestUpdateDashboard_KubernetesObjectFormCreatesV2(t *testing.T) {
	var posted map[string]interface{}
	ts := httptest.NewServer(withFrontendSettings("default", func(w http.ResponseWriter, r *http.Request) {
		if serveDashboardDiscovery(w, r, "v1beta1", "v2beta1") {
			return
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"metadata": map[string]interface{}{"name": "my-dash"}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	k8s := &mcpgrafana.KubernetesClient{BaseURL: ts.URL, HTTPClient: ts.Client()}
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{URL: ts.URL})
	ctx = mcpgrafana.WithKubernetesClient(ctx, k8s)

	res, err := updateDashboard(ctx, UpdateDashboardParams{
		Dashboard: k8sDashboardObject(map[string]interface{}{"title": "t", "elements": map[string]interface{}{}}),
	})
	require.NoError(t, err)
	assert.Equal(t, "my-dash", *res.UID)
	require.NotNil(t, posted)
	assert.Equal(t, "dashboard.grafana.app/v2beta1", posted["apiVersion"])
	assert.Equal(t, "my-dash", k8sNestedString(posted, "metadata", "name"))
	assert.Equal(t, "folder-a", k8sNestedString(posted, "metadata", "annotations", "grafana.app/folder"))
	spec, _ := posted["spec"].(map[string]interface{})
	assert.Equal(t, "t", spec["title"])
	assert.NotContains(t, spec, "uid")
}
