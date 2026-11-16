package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	aapi "github.com/grafana/amixr-api-go-client"
	mcpgrafana "github.com/grafana/mcp-grafana/v2"
	"github.com/stretchr/testify/require"
)

// getAlertGroupServiceFromContext builds an AlertGroupService on the OnCall
// client the tools use, so tests can observe the auth header it sends.
func getAlertGroupServiceFromContext(ctx context.Context) (*aapi.AlertGroupService, error) {
	client, err := oncallClientFromContext(ctx)
	if err != nil {
		return nil, err
	}
	return aapi.NewAlertGroupService(client), nil
}

// newTestServers creates a fake OnCall API server and a fake Grafana server.
// The OnCall server captures the Authorization header from each request.
// The Grafana server returns the OnCall URL from the IRM plugin settings.
// Returns the Grafana server URL and a mutex-protected getter for the captured auth header.
func newTestServers(t *testing.T) (grafanaURL string, getAuthHeader func() string) {
	t.Helper()

	var mu sync.Mutex
	var capturedAuthHeader string

	oncallServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedAuthHeader = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"previous":null,"results":[]}`))
	}))
	t.Cleanup(oncallServer.Close)

	grafanaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"jsonData": map[string]interface{}{
				"onCallApiUrl": oncallServer.URL,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(grafanaServer.Close)

	getAuthHeader = func() string {
		mu.Lock()
		defer mu.Unlock()
		return capturedAuthHeader
	}

	return grafanaServer.URL, getAuthHeader
}

func TestOncallClientFromContext_UsesOnCallTokenWhenSet(t *testing.T) {
	grafanaURL, getAuthHeader := newTestServers(t)

	config := mcpgrafana.GrafanaConfig{
		URL:         grafanaURL,
		APIKey:      "sa-token",
		OnCallToken: "personal-oncall-token",
	}
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), config)

	svc, err := getAlertGroupServiceFromContext(ctx)
	require.NoError(t, err)

	_, _, err = svc.ListAlertGroups(&aapi.ListAlertGroupOptions{})
	require.NoError(t, err)

	require.Equal(t, "personal-oncall-token", getAuthHeader())
}

func TestOncallClientFromContext_FallsBackToAPIKey(t *testing.T) {
	grafanaURL, getAuthHeader := newTestServers(t)

	config := mcpgrafana.GrafanaConfig{
		URL:         grafanaURL,
		APIKey:      "sa-token",
		OnCallToken: "",
	}
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), config)

	svc, err := getAlertGroupServiceFromContext(ctx)
	require.NoError(t, err)

	_, _, err = svc.ListAlertGroups(&aapi.ListAlertGroupOptions{})
	require.NoError(t, err)

	require.Equal(t, "sa-token", getAuthHeader())
}

func TestOncallClientFromContext_OnCallTokenSetAPIKeyEmpty(t *testing.T) {
	grafanaURL, getAuthHeader := newTestServers(t)

	config := mcpgrafana.GrafanaConfig{
		URL:         grafanaURL,
		APIKey:      "",
		OnCallToken: "personal-oncall-token",
	}
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), config)

	svc, err := getAlertGroupServiceFromContext(ctx)
	require.NoError(t, err)

	_, _, err = svc.ListAlertGroups(&aapi.ListAlertGroupOptions{})
	require.NoError(t, err)

	require.Equal(t, "personal-oncall-token", getAuthHeader())
}

func TestOncallClientFromContext_BothTokensEmpty(t *testing.T) {
	grafanaURL, _ := newTestServers(t)

	config := mcpgrafana.GrafanaConfig{
		URL:         grafanaURL,
		APIKey:      "",
		OnCallToken: "",
	}
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), config)

	_, err := oncallClientFromContext(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no OnCall authentication token")
}

func TestOncallClientFromContext_WhitespaceOnCallToken(t *testing.T) {
	grafanaURL, _ := newTestServers(t)

	// Whitespace-only token is not empty string, so our code treats it as "set"
	// and does NOT fall back to APIKey. The whitespace token will be sent as-is
	// in the Authorization header and rejected by the OnCall API at runtime.
	config := mcpgrafana.GrafanaConfig{
		URL:         grafanaURL,
		APIKey:      "sa-token",
		OnCallToken: "   ",
	}
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), config)

	// Client creation should succeed (validation happens server-side).
	_, err := oncallClientFromContext(ctx)
	require.NoError(t, err)
}

// TestGetAlertGroup_SurfacesDetailFields proves the wrapper preserves fields
// that the upstream amixr-api-go-client AlertGroup struct drops:
// acknowledged_by, resolved_by, silenced_at, last_alert.
func TestGetAlertGroup_SurfacesDetailFields(t *testing.T) {
	detailJSON := `{
		"id": "I12345",
		"integration_id": "C1",
		"route_id": "R1",
		"alerts_count": 3,
		"state": "firing",
		"created_at": "2026-05-01T00:00:00Z",
		"resolved_at": "",
		"acknowledged_at": "2026-05-01T01:00:00Z",
		"title": "TestAlert",
		"permalinks": {"slack": "https://example/slack"},
		"acknowledged_by": "user-1",
		"resolved_by": "",
		"silenced_at": "",
		"last_alert": {
			"id": "A1",
			"created_at": "2026-05-01T02:00:00Z",
			"payload": {"ruleName": "TestRule", "message": "hello", "labels": {"severity": "high"}}
		}
	}`

	oncallServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/alert_groups/I12345/" {
			_, _ = w.Write([]byte(detailJSON))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(oncallServer.Close)

	grafanaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonData": map[string]interface{}{"onCallApiUrl": oncallServer.URL},
		})
	}))
	t.Cleanup(grafanaServer.Close)

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    grafanaServer.URL,
		APIKey: "sa-token",
	})

	result, err := getAlertGroup(ctx, GetAlertGroupParams{AlertGroupID: "I12345"})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, "I12345", result.ID)
	require.Equal(t, "user-1", result.AcknowledgedBy)
	require.Equal(t, "", result.ResolvedBy)
	require.Equal(t, "", result.SilencedAt)
	require.NotNil(t, result.LastAlert)
	require.Equal(t, "A1", result.LastAlert.ID)
	require.Equal(t, "2026-05-01T02:00:00Z", result.LastAlert.CreatedAt)

	require.Equal(t, "R1", result.RouteID)
	payload := result.LastAlert.Payload
	require.Equal(t, "TestRule", payload["ruleName"])
	require.Equal(t, "hello", payload["message"])
}

func TestGetAlertGroup_EmptyID(t *testing.T) {
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    "http://unused",
		APIKey: "sa-token",
	})
	_, err := getAlertGroup(ctx, GetAlertGroupParams{AlertGroupID: "   "})
	require.Error(t, err)
	require.Contains(t, err.Error(), "alertGroupId is required")
}

// TestSilenceAlertGroup_PostsDelayWithOnCallToken checks a fork action tool
// end to end: POST alert_groups/<id>/silence/ with the delay body, sent with
// the OnCall personal token, then the alert group read back.
func TestSilenceAlertGroup_PostsDelayWithOnCallToken(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotAuth string
	var gotBody map[string]int
	oncallServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			mu.Lock()
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			mu.Unlock()
			return
		}
		_, _ = w.Write([]byte(`{"id":"I1","state":"silenced"}`))
	}))
	t.Cleanup(oncallServer.Close)
	grafanaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonData": map[string]interface{}{"onCallApiUrl": oncallServer.URL}})
	}))
	t.Cleanup(grafanaServer.Close)

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL: grafanaServer.URL, APIKey: "sa-token", OnCallToken: "personal-token",
	})
	ag, err := silenceAlertGroup(ctx, SilenceAlertGroupParams{AlertGroupID: "I1", Delay: 3600})
	require.NoError(t, err)
	require.Equal(t, "silenced", ag.State)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "/api/v1/alert_groups/I1/silence/", gotPath)
	require.Equal(t, "personal-token", gotAuth)
	require.Equal(t, map[string]int{"delay": 3600}, gotBody)
}
