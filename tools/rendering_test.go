//go:build unit

package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcpgrafana "github.com/grafana/mcp-grafana"
)

func intPtr(i int) *int {
	return &i
}

func TestStringOrSlice_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected StringOrSlice
		wantErr  bool
	}{
		{
			name:     "Single string value",
			input:    `"prometheus"`,
			expected: StringOrSlice{"prometheus"},
		},
		{
			name:     "Array with single value",
			input:    `["prometheus"]`,
			expected: StringOrSlice{"prometheus"},
		},
		{
			name:     "Array with multiple values",
			input:    `["server1", "server2", "server3"]`,
			expected: StringOrSlice{"server1", "server2", "server3"},
		},
		{
			name:     "Empty array",
			input:    `[]`,
			expected: StringOrSlice{},
		},
		{
			name:    "Invalid type (number)",
			input:   `42`,
			wantErr: true,
		},
		{
			name:    "Invalid type (object)",
			input:   `{"key": "value"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result StringOrSlice
			err := json.Unmarshal([]byte(tt.input), &result)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestStringOrSlice_MarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    StringOrSlice
		expected string
	}{
		{
			name:     "Single value marshals as string",
			input:    StringOrSlice{"prometheus"},
			expected: `"prometheus"`,
		},
		{
			name:     "Multiple values marshal as array",
			input:    StringOrSlice{"server1", "server2"},
			expected: `["server1","server2"]`,
		},
		{
			name:     "Empty slice marshals as empty array",
			input:    StringOrSlice{},
			expected: `[]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := json.Marshal(tt.input)
			require.NoError(t, err)
			assert.JSONEq(t, tt.expected, string(result))
		})
	}
}

func TestGetPanelImageParams_UnmarshalVariables(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]StringOrSlice
	}{
		{
			name:  "Single string values (backward compatible)",
			input: `{"dashboardUid": "abc", "variables": {"var-datasource": "prometheus", "var-host": "server01"}}`,
			expected: map[string]StringOrSlice{
				"var-datasource": {"prometheus"},
				"var-host":       {"server01"},
			},
		},
		{
			name:  "Multi-value array",
			input: `{"dashboardUid": "abc", "variables": {"var-instance": ["172.16.31.129", "172.16.32.99"]}}`,
			expected: map[string]StringOrSlice{
				"var-instance": {"172.16.31.129", "172.16.32.99"},
			},
		},
		{
			name:  "Mixed single and multi-value",
			input: `{"dashboardUid": "abc", "variables": {"var-datasource": "prometheus", "var-instance": ["server1", "server2"]}}`,
			expected: map[string]StringOrSlice{
				"var-datasource": {"prometheus"},
				"var-instance":   {"server1", "server2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params GetPanelImageParams
			err := json.Unmarshal([]byte(tt.input), &params)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, params.Variables)
		})
	}
}

func TestBuildRenderURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		args     GetPanelImageParams
		contains []string
	}{
		{
			name:    "Basic dashboard render",
			baseURL: "http://localhost:3000",
			args: GetPanelImageParams{
				DashboardUID: "abc123",
			},
			contains: []string{
				"http://localhost:3000/render/d/abc123",
				"width=1000",
				"height=500",
				"scale=1",
				"kiosk=true",
			},
		},
		{
			name:    "Panel render with custom dimensions",
			baseURL: "http://localhost:3000",
			args: GetPanelImageParams{
				DashboardUID: "abc123",
				PanelID:      intPtr(5),
				Width:        intPtr(800),
				Height:       intPtr(600),
			},
			contains: []string{
				"http://localhost:3000/render/d/abc123",
				"viewPanel=5",
				"width=800",
				"height=600",
			},
		},
		{
			name:    "With time range",
			baseURL: "http://localhost:3000",
			args: GetPanelImageParams{
				DashboardUID: "abc123",
				TimeRange: &RenderTimeRange{
					From: "now-1h",
					To:   "now",
				},
			},
			contains: []string{
				"from=now-1h",
				"to=now",
			},
		},
		{
			name:    "With theme",
			baseURL: "http://localhost:3000",
			args: GetPanelImageParams{
				DashboardUID: "abc123",
				Theme:        stringPtr("light"),
			},
			contains: []string{
				"theme=light",
			},
		},
		{
			name:    "With single-value variables",
			baseURL: "http://localhost:3000",
			args: GetPanelImageParams{
				DashboardUID: "abc123",
				Variables: map[string]StringOrSlice{
					"var-datasource": {"prometheus"},
					"var-host":       {"server01"},
				},
			},
			contains: []string{
				"var-datasource=prometheus",
				"var-host=server01",
			},
		},
		{
			name:    "With multi-value variables",
			baseURL: "http://localhost:3000",
			args: GetPanelImageParams{
				DashboardUID: "abc123",
				Variables: map[string]StringOrSlice{
					"var-instance": {"172.16.31.129", "172.16.32.99"},
				},
			},
			contains: []string{
				"var-instance=172.16.31.129",
				"var-instance=172.16.32.99",
			},
		},
		{
			name:    "With scale",
			baseURL: "http://localhost:3000",
			args: GetPanelImageParams{
				DashboardUID: "abc123",
				Scale:        intPtr(2),
			},
			contains: []string{
				"scale=2",
			},
		},
		{
			name:    "URL with trailing slash",
			baseURL: "http://localhost:3000/",
			args: GetPanelImageParams{
				DashboardUID: "abc123",
			},
			contains: []string{
				"http://localhost:3000/render/d/abc123",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildRenderURL(tt.baseURL, tt.args)
			require.NoError(t, err)

			for _, expected := range tt.contains {
				assert.Contains(t, result, expected)
			}
		})
	}
}

func TestGetPanelImage(t *testing.T) {
	// Create a test PNG image (1x1 pixel)
	testPNGData := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, // IHDR chunk
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xD7, 0x63, 0xF8, 0xFF, 0xFF, 0x3F,
		0x00, 0x05, 0xFE, 0x02, 0xFE, 0xDC, 0xCC, 0x59,
		0xE7, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
		0x44, 0xAE, 0x42, 0x60, 0x82, // IEND chunk
	}

	t.Run("Successful image render", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Contains(t, r.URL.Path, "/render/d/test-dash")
			assert.Equal(t, "1000", r.URL.Query().Get("width"))
			assert.Equal(t, "500", r.URL.Query().Get("height"))

			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(testPNGData)
		}))
		defer server.Close()

		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL:    server.URL,
			APIKey: "test-api-key",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		result, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
		})

		require.NoError(t, err)
		require.NotNil(t, result)
		require.Len(t, result.Content, 1)

		// Check that the content is image content with base64 data
		content := result.Content[0]
		imageContent, ok := content.(interface {
			GetData() string
			GetMimeType() string
		})
		if ok {
			assert.Equal(t, "image/png", imageContent.GetMimeType())
			// Verify base64 decoding works
			decoded, err := base64.StdEncoding.DecodeString(imageContent.GetData())
			require.NoError(t, err)
			assert.Equal(t, testPNGData, decoded)
		}
	})

	t.Run("Panel image with specific panel ID", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "5", r.URL.Query().Get("viewPanel"))

			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(testPNGData)
		}))
		defer server.Close()

		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL:    server.URL,
			APIKey: "test-api-key",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		panelID := 5
		result, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
			PanelID:      &panelID,
		})

		require.NoError(t, err)
		require.NotNil(t, result)
	})

	t.Run("Authentication header with API key", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer test-api-key", r.Header.Get("Authorization"))

			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(testPNGData)
		}))
		defer server.Close()

		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL:    server.URL,
			APIKey: "test-api-key",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		_, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
		})

		require.NoError(t, err)
	})

	t.Run("Image renderer not available returns helpful error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Not Found"))
		}))
		defer server.Close()

		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL:    server.URL,
			APIKey: "test-api-key",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		_, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "image renderer not available")
		assert.Contains(t, err.Error(), "Grafana Image Renderer service")
	})

	t.Run("Server error returns error message", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("Internal Server Error"))
		}))
		defer server.Close()

		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL:    server.URL,
			APIKey: "test-api-key",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		_, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "HTTP 500")
	})

	t.Run("Missing Grafana URL returns error", func(t *testing.T) {
		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL: "",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		_, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "grafana URL not configured")
	})

	t.Run("With time range parameters", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "now-1h", r.URL.Query().Get("from"))
			assert.Equal(t, "now", r.URL.Query().Get("to"))

			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(testPNGData)
		}))
		defer server.Close()

		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL:    server.URL,
			APIKey: "test-api-key",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		_, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
			TimeRange: &RenderTimeRange{
				From: "now-1h",
				To:   "now",
			},
		})

		require.NoError(t, err)
	})

	t.Run("With dashboard variables", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "prometheus", r.URL.Query().Get("var-datasource"))

			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(testPNGData)
		}))
		defer server.Close()

		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL:    server.URL,
			APIKey: "test-api-key",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		_, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
			Variables: map[string]StringOrSlice{
				"var-datasource": {"prometheus"},
			},
		})

		require.NoError(t, err)
	})

	t.Run("With multi-value dashboard variables", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Multi-value variables should appear as multiple query params
			values := r.URL.Query()["var-instance"]
			assert.ElementsMatch(t, []string{"172.16.31.129", "172.16.32.99"}, values)

			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(testPNGData)
		}))
		defer server.Close()

		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL:    server.URL,
			APIKey: "test-api-key",
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		_, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
			Variables: map[string]StringOrSlice{
				"var-instance": {"172.16.31.129", "172.16.32.99"},
			},
		})

		require.NoError(t, err)
	})

	t.Run("Org ID header is set when configured", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "2", r.Header.Get("X-Grafana-Org-Id"))

			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(testPNGData)
		}))
		defer server.Close()

		grafanaCfg := mcpgrafana.GrafanaConfig{
			URL:    server.URL,
			APIKey: "test-api-key",
			OrgID:  2,
		}
		ctx := mcpgrafana.WithGrafanaConfig(context.Background(), grafanaCfg)

		_, err := getPanelImage(ctx, GetPanelImageParams{
			DashboardUID: "test-dash",
		})

		require.NoError(t, err)
	})
}

// newImageServer returns a fake Grafana /render endpoint that always serves
// testPNGData. The shared helper keeps the output-format tests focused on the
// content packaging rather than on repeating HTTP plumbing.
func newImageServer(t *testing.T, testPNGData []byte, contentType string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testPNGData)
	}))
}

func TestResolveOutputFormat(t *testing.T) {
	tests := []struct {
		name    string
		input   *string
		want    string
		wantErr bool
	}{
		{name: "nil → default", input: nil, want: OutputFormatImage},
		{name: "empty → default", input: stringPtr(""), want: OutputFormatImage},
		{name: "whitespace-only → default", input: stringPtr("   "), want: OutputFormatImage},
		{name: "image literal", input: stringPtr("image"), want: OutputFormatImage},
		{name: "resource literal", input: stringPtr("resource"), want: OutputFormatResource},
		{name: "both literal", input: stringPtr("both"), want: OutputFormatBoth},
		{name: "case-insensitive", input: stringPtr("Resource"), want: OutputFormatResource},
		{name: "surrounding whitespace", input: stringPtr("  both  "), want: OutputFormatBoth},
		{name: "unknown value errors", input: stringPtr("binary"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveOutputFormat(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "invalid outputFormat")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildRenderResourceURI(t *testing.T) {
	tests := []struct {
		name string
		args GetPanelImageParams
		want string
	}{
		{
			name: "dashboard only",
			args: GetPanelImageParams{DashboardUID: "abc123"},
			want: "grafana://render/dashboard/abc123",
		},
		{
			name: "panel included",
			args: GetPanelImageParams{DashboardUID: "abc123", PanelID: intPtr(7)},
			want: "grafana://render/dashboard/abc123/panel/7",
		},
		{
			name: "uid with special chars is escaped",
			args: GetPanelImageParams{DashboardUID: "has/slash and space"},
			want: "grafana://render/dashboard/has%2Fslash%20and%20space",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, buildRenderResourceURI(tt.args))
		})
	}
}

// TestGetPanelImage_OutputFormats exercises the full HTTP path for each
// supported outputFormat, proving that the wire-level Content slice matches
// what downstream hooks rely on (ImageContent for display, BlobResourceContents
// for reliable decode out-of-band).
func TestGetPanelImage_OutputFormats(t *testing.T) {
	testPNGData := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xD7, 0x63, 0xF8, 0xFF, 0xFF, 0x3F,
		0x00, 0x05, 0xFE, 0x02, 0xFE, 0xDC, 0xCC, 0x59,
		0xE7, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
		0x44, 0xAE, 0x42, 0x60, 0x82,
	}
	wantB64 := base64.StdEncoding.EncodeToString(testPNGData)

	type assertion func(t *testing.T, result *mcp.CallToolResult)

	assertImageOnly := func(t *testing.T, result *mcp.CallToolResult) {
		t.Helper()
		require.Len(t, result.Content, 1)
		img, ok := result.Content[0].(mcp.ImageContent)
		require.True(t, ok, "first content must be ImageContent, got %T", result.Content[0])
		assert.Equal(t, "image", img.Type)
		assert.Equal(t, "image/png", img.MIMEType)
		assert.Equal(t, wantB64, img.Data)
	}

	assertResourceOnly := func(t *testing.T, result *mcp.CallToolResult) {
		t.Helper()
		require.Len(t, result.Content, 1)
		res, ok := result.Content[0].(mcp.EmbeddedResource)
		require.True(t, ok, "first content must be EmbeddedResource, got %T", result.Content[0])
		assert.Equal(t, "resource", res.Type)
		blob, ok := res.Resource.(mcp.BlobResourceContents)
		require.True(t, ok, "resource payload must be BlobResourceContents, got %T", res.Resource)
		assert.Equal(t, "image/png", blob.MIMEType)
		assert.Equal(t, wantB64, blob.Blob)
		assert.Equal(t, "grafana://render/dashboard/test-dash", blob.URI)
	}

	assertBoth := func(t *testing.T, result *mcp.CallToolResult) {
		t.Helper()
		require.Len(t, result.Content, 2, "'both' must emit ImageContent first, EmbeddedResource second")
		img, ok := result.Content[0].(mcp.ImageContent)
		require.True(t, ok, "first content must be ImageContent, got %T", result.Content[0])
		assert.Equal(t, wantB64, img.Data)
		res, ok := result.Content[1].(mcp.EmbeddedResource)
		require.True(t, ok, "second content must be EmbeddedResource, got %T", result.Content[1])
		blob, ok := res.Resource.(mcp.BlobResourceContents)
		require.True(t, ok)
		assert.Equal(t, wantB64, blob.Blob)
	}

	cases := []struct {
		name   string
		format *string
		check  assertion
	}{
		{name: "default (nil) → image", format: nil, check: assertImageOnly},
		{name: "explicit image", format: stringPtr("image"), check: assertImageOnly},
		{name: "resource only", format: stringPtr("resource"), check: assertResourceOnly},
		{name: "both", format: stringPtr("both"), check: assertBoth},
		// Verify normalization without adding a separate HTTP round-trip per
		// case — these cover the trim/lowercase branches of resolveOutputFormat
		// *through* the public entry point.
		{name: "both (uppercase)", format: stringPtr("BOTH"), check: assertBoth},
		{name: "resource (whitespace)", format: stringPtr("  resource  "), check: assertResourceOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newImageServer(t, testPNGData, "image/png")
			defer server.Close()

			ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
				URL:    server.URL,
				APIKey: "test-api-key",
			})

			result, err := getPanelImage(ctx, GetPanelImageParams{
				DashboardUID: "test-dash",
				OutputFormat: tc.format,
			})
			require.NoError(t, err)
			require.NotNil(t, result)
			tc.check(t, result)
		})
	}
}

// TestGetPanelImage_InvalidOutputFormat checks that we fail loudly on typos
// rather than silently falling back to the default — matters because the tool
// description advertises enum-style values and callers rely on us rejecting
// anything else.
func TestGetPanelImage_InvalidOutputFormat(t *testing.T) {
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    "http://example.invalid",
		APIKey: "test-api-key",
	})

	_, err := getPanelImage(ctx, GetPanelImageParams{
		DashboardUID: "test-dash",
		OutputFormat: stringPtr("pdf"),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid outputFormat")
	assert.Contains(t, err.Error(), `"pdf"`)
	assert.Contains(t, err.Error(), "image")
	assert.Contains(t, err.Error(), "resource")
	assert.Contains(t, err.Error(), "both")
}

// TestGetPanelImage_PanelURIIncludesPanelID ensures EmbeddedResource URIs for
// panel-scoped renders carry the panel ID, so callers can distinguish them
// from whole-dashboard renders in logs and caches.
func TestGetPanelImage_PanelURIIncludesPanelID(t *testing.T) {
	testPNGData := []byte{0x89, 0x50, 0x4E, 0x47}
	server := newImageServer(t, testPNGData, "image/png")
	defer server.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    server.URL,
		APIKey: "test-api-key",
	})

	result, err := getPanelImage(ctx, GetPanelImageParams{
		DashboardUID: "test-dash",
		PanelID:      intPtr(42),
		OutputFormat: stringPtr("resource"),
	})
	require.NoError(t, err)

	res := result.Content[0].(mcp.EmbeddedResource)
	blob := res.Resource.(mcp.BlobResourceContents)
	assert.Equal(t, "grafana://render/dashboard/test-dash/panel/42", blob.URI)
}

// TestGetPanelImage_MIMETypeFromResponse verifies we trust the Content-Type
// from Grafana when it is a valid image/* type, and fall back to image/png
// otherwise. Prevents regressions where non-PNG renders (JPEG via a proxy)
// would be mislabeled in the resource blob.
func TestGetPanelImage_MIMETypeFromResponse(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		wantMIME    string
	}{
		{name: "png passthrough", contentType: "image/png", wantMIME: "image/png"},
		{name: "jpeg passthrough", contentType: "image/jpeg", wantMIME: "image/jpeg"},
		{name: "missing falls back to png", contentType: "", wantMIME: "image/png"},
		{name: "non-image falls back to png", contentType: "application/json", wantMIME: "image/png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testPNGData := []byte{0x89, 0x50, 0x4E, 0x47}
			server := newImageServer(t, testPNGData, tt.contentType)
			defer server.Close()

			ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
				URL:    server.URL,
				APIKey: "test-api-key",
			})

			result, err := getPanelImage(ctx, GetPanelImageParams{
				DashboardUID: "test-dash",
				OutputFormat: stringPtr("both"),
			})
			require.NoError(t, err)
			require.Len(t, result.Content, 2)

			img := result.Content[0].(mcp.ImageContent)
			assert.Equal(t, tt.wantMIME, img.MIMEType)

			res := result.Content[1].(mcp.EmbeddedResource)
			blob := res.Resource.(mcp.BlobResourceContents)
			assert.Equal(t, tt.wantMIME, blob.MIMEType)
		})
	}
}

// TestGetPanelImageParams_UnmarshalOutputFormat locks in the JSON contract
// that callers (skills, hooks, integration tests) rely on.
func TestGetPanelImageParams_UnmarshalOutputFormat(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  *string
	}{
		{name: "omitted", input: `{"dashboardUid":"x"}`, want: nil},
		{name: "null", input: `{"dashboardUid":"x","outputFormat":null}`, want: nil},
		{name: "image", input: `{"dashboardUid":"x","outputFormat":"image"}`, want: stringPtr("image")},
		{name: "resource", input: `{"dashboardUid":"x","outputFormat":"resource"}`, want: stringPtr("resource")},
		{name: "both", input: `{"dashboardUid":"x","outputFormat":"both"}`, want: stringPtr("both")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params GetPanelImageParams
			require.NoError(t, json.Unmarshal([]byte(tt.input), &params))
			if tt.want == nil {
				assert.Nil(t, params.OutputFormat)
				return
			}
			require.NotNil(t, params.OutputFormat)
			assert.Equal(t, *tt.want, *params.OutputFormat)
		})
	}
}
