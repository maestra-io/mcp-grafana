package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	aapi "github.com/grafana/amixr-api-go-client"
	mcpgrafana "github.com/grafana/mcp-grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOncallClientFromContext_TokenPriority(t *testing.T) {
	// capturedAuthHeader records the Authorization header from the last request.
	var capturedAuthHeader string

	// Fake OnCall API that captures the Authorization header.
	oncallServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"previous":null,"results":[]}`))
	}))
	defer oncallServer.Close()

	// Fake Grafana that returns the OnCall URL from the IRM plugin settings.
	grafanaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"jsonData": map[string]interface{}{
				"onCallApiUrl": oncallServer.URL,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer grafanaServer.Close()

	t.Run("uses OnCallToken when set", func(t *testing.T) {
		capturedAuthHeader = ""
		config := mcpgrafana.GrafanaConfig{
			URL:         grafanaServer.URL,
			APIKey:      "sa-token",
			OnCallToken: "personal-oncall-token",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), config)

		svc, err := getAlertGroupServiceFromContext(ctx)
		require.NoError(t, err)

		// Make a request so the token gets sent.
		_, _, err = svc.ListAlertGroups(&aapi.ListAlertGroupOptions{})
		require.NoError(t, err)

		assert.Equal(t, "personal-oncall-token", capturedAuthHeader)
	})

	t.Run("falls back to APIKey when OnCallToken is empty", func(t *testing.T) {
		capturedAuthHeader = ""
		config := mcpgrafana.GrafanaConfig{
			URL:         grafanaServer.URL,
			APIKey:      "sa-token",
			OnCallToken: "",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), config)

		svc, err := getAlertGroupServiceFromContext(ctx)
		require.NoError(t, err)

		_, _, err = svc.ListAlertGroups(&aapi.ListAlertGroupOptions{})
		require.NoError(t, err)

		assert.Equal(t, "sa-token", capturedAuthHeader)
	})
}
