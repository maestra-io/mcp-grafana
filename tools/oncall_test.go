package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	aapi "github.com/grafana/amixr-api-go-client"
	mcpgrafana "github.com/grafana/mcp-grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServers creates a fake OnCall API server and a fake Grafana server.
// The OnCall server captures the Authorization header from each request.
// The Grafana server returns the OnCall URL from the IRM plugin settings.
// Returns the servers, a mutex-protected getter for the captured header, and a cleanup func.
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
	assert.Contains(t, err.Error(), "no authentication token configured for OnCall")
}

func TestOncallClientFromContext_WhitespaceOnCallToken(t *testing.T) {
	grafanaURL, _ := newTestServers(t)

	// Whitespace-only token is not empty string, so our code treats it as "set"
	// and does NOT fall back to APIKey. The amixr HTTP client trims headers,
	// so the actual Authorization header will be empty — but the important
	// thing is that we don't fall back to the SA token.
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
