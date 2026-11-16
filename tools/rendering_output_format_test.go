//go:build unit

package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcpgrafana "github.com/grafana/mcp-grafana/v2"
)

// Tests for the maestra-io fork's get_panel_image outputFormat and bounded
// render reads.

// imageBlocks drops the trailing deeplink TextContent upstream appends after
// the image blocks, so the tests below check the fork's packaging alone.
func imageBlocks(content []mcp.Content) []mcp.Content {
	out := make([]mcp.Content, 0, len(content))
	for _, c := range content {
		if txt, ok := c.(*mcp.TextContent); ok && txt.Meta != nil {
			continue
		}
		out = append(out, c)
	}
	return out
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
		{name: "nil -> default", input: nil, want: outputFormatImage},
		{name: "image literal", input: stringPtr("image"), want: outputFormatImage},
		{name: "resource literal", input: stringPtr("resource"), want: outputFormatResource},
		{name: "both literal", input: stringPtr("both"), want: outputFormatBoth},
		{name: "text_base64 literal", input: stringPtr("text_base64"), want: outputFormatTextBase64},
		// Schema advertises lowercase only; we reject deviations so a
		// client that validates against the enum sees the same behavior
		// as the server. Explicit "" is a caller bug, not a "don't care"
		// signal — the pointer being nil is the only valid "don't care".
		{name: "empty string rejected", input: stringPtr(""), wantErr: true},
		{name: "uppercase rejected", input: stringPtr("BOTH"), wantErr: true},
		{name: "whitespace rejected", input: stringPtr(" both "), wantErr: true},
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
	// Pinned so the test fails loudly if the hashing scheme ever changes
	// accidentally. Computed as hex.EncodeToString(sha256("payload")[:16]).
	const payload = "payload"
	const wantHash = "239f59ed55e737c77147cf55ad0c1b03"

	tests := []struct {
		name string
		args GetPanelImageParams
		want string
	}{
		{
			name: "dashboard only",
			args: GetPanelImageParams{DashboardUID: "abc123"},
			want: "grafana://render/dashboard/abc123?h=" + wantHash,
		},
		{
			name: "panel included",
			args: GetPanelImageParams{DashboardUID: "abc123", PanelID: intPtr(7)},
			want: "grafana://render/dashboard/abc123/panel/7?h=" + wantHash,
		},
		{
			name: "uid with special chars is escaped",
			args: GetPanelImageParams{DashboardUID: "has/slash and space"},
			want: "grafana://render/dashboard/has%2Fslash%20and%20space?h=" + wantHash,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, buildRenderResourceURI(tt.args, []byte(payload)))
		})
	}
}

// TestBuildRenderResourceURI_DistinctContentYieldsDistinctURI locks in the
// dedup-disambiguation contract: two renders of the same dashboard with
// different pixel bytes must produce different URIs so MCP clients that
// dedupe by URI don't collapse them into one.
func TestBuildRenderResourceURI_DistinctContentYieldsDistinctURI(t *testing.T) {
	args := GetPanelImageParams{DashboardUID: "abc"}
	uri1 := buildRenderResourceURI(args, []byte("render-1"))
	uri2 := buildRenderResourceURI(args, []byte("render-2"))
	assert.NotEqual(t, uri1, uri2, "distinct image bytes must yield distinct URIs")
}

// TestBuildRenderResourceURI_StableForSameContent locks in the
// idempotency contract: re-rendering the identical state produces the
// identical URI, letting well-behaved caches treat it as a cache key.
func TestBuildRenderResourceURI_StableForSameContent(t *testing.T) {
	args := GetPanelImageParams{DashboardUID: "abc", PanelID: intPtr(3)}
	uri1 := buildRenderResourceURI(args, []byte("render-1"))
	uri2 := buildRenderResourceURI(args, []byte("render-1"))
	assert.Equal(t, uri1, uri2, "identical image bytes must yield identical URIs")
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
		require.Len(t, imageBlocks(result.Content), 1)
		img, ok := imageBlocks(result.Content)[0].(*mcp.ImageContent)
		require.True(t, ok, "first content must be ImageContent, got %T", imageBlocks(result.Content)[0])
		assert.Equal(t, "image/png", img.MIMEType)
		assert.Equal(t, testPNGData, img.Data)
	}

	assertResourceOnly := func(t *testing.T, result *mcp.CallToolResult) {
		t.Helper()
		require.Len(t, imageBlocks(result.Content), 1)
		res, ok := imageBlocks(result.Content)[0].(*mcp.EmbeddedResource)
		require.True(t, ok, "first content must be EmbeddedResource, got %T", imageBlocks(result.Content)[0])
		blob := res.Resource
		require.NotNil(t, blob)
		assert.Equal(t, "image/png", blob.MIMEType)
		assert.Equal(t, testPNGData, blob.Blob)
		assert.True(t, strings.HasPrefix(blob.URI, "grafana://render/dashboard/test-dash?h="),
			"resource URI must be dashboard-scoped with a content hash, got %q", blob.URI)
		// The resource block should be annotated so context-pruning clients
		// know it's out-of-band tooling payload, not something to feed the LLM.
		require.NotNil(t, res.Annotations)
		assert.Equal(t, []mcp.Role{mcp.Role("user")}, res.Annotations.Audience)
	}

	assertBoth := func(t *testing.T, result *mcp.CallToolResult) {
		t.Helper()
		require.Len(t, imageBlocks(result.Content), 2, "'both' must emit ImageContent first, EmbeddedResource second")
		img, ok := imageBlocks(result.Content)[0].(*mcp.ImageContent)
		require.True(t, ok, "first content must be ImageContent, got %T", imageBlocks(result.Content)[0])
		assert.Equal(t, testPNGData, img.Data)
		res, ok := imageBlocks(result.Content)[1].(*mcp.EmbeddedResource)
		require.True(t, ok, "second content must be EmbeddedResource, got %T", imageBlocks(result.Content)[1])
		blob := res.Resource
		require.NotNil(t, blob)
		assert.Equal(t, testPNGData, blob.Blob)
	}

	assertTextBase64 := func(t *testing.T, result *mcp.CallToolResult) {
		t.Helper()
		require.Len(t, imageBlocks(result.Content), 1, "text_base64 must emit exactly one TextContent block")
		txt, ok := imageBlocks(result.Content)[0].(*mcp.TextContent)
		require.True(t, ok, "content must be TextContent, got %T", imageBlocks(result.Content)[0])
		// Must be ONLY the base64 — no prefix, suffix, or wrapper. Any
		// stray characters would corrupt a downstream `base64 -d` pipe.
		assert.Equal(t, wantB64, txt.Text)
		// No line wrapping either — the shell pipe uses single-line input.
		assert.NotContains(t, txt.Text, "\n", "base64 must be single-line for shell pipe")
		assert.NotContains(t, txt.Text, "\r", "base64 must be single-line for shell pipe")
		// Annotations deliberately left nil: MCP annotations are hints,
		// and a client that interprets audience=[assistant] as "drop
		// from context" would silently defeat the whole point of this
		// mode. Pin the default-audience contract so a future maintainer
		// doesn't "fix" the asymmetry by copy-pasting the image/resource
		// annotations onto this branch.
		assert.Nil(t, txt.Annotations, "TextContent must use default audience so the model reliably sees the base64")
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
		{name: "text_base64", format: stringPtr("text_base64"), check: assertTextBase64},
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
	assert.Contains(t, err.Error(), "text_base64")
}

// TestGetPanelImage_TextBase64_DecodesToOriginalBytes locks in the contract
// that callers rely on when they pipe the returned text through `base64 -d`:
// the TextContent.Text field must be exactly the base64 encoding of the
// image bytes, with no prose, prefix, or trailing whitespace that would
// make the decoded output garbage.
func TestGetPanelImage_TextBase64_DecodesToOriginalBytes(t *testing.T) {
	testPNGData := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0xde, 0xad, 0xbe, 0xef}
	server := newImageServer(t, testPNGData, "image/png")
	defer server.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    server.URL,
		APIKey: "test-api-key",
	})

	result, err := getPanelImage(ctx, GetPanelImageParams{
		DashboardUID: "test-dash",
		OutputFormat: stringPtr("text_base64"),
	})
	require.NoError(t, err)
	require.Len(t, imageBlocks(result.Content), 1)

	txt := imageBlocks(result.Content)[0].(*mcp.TextContent)
	decoded, err := base64.StdEncoding.DecodeString(txt.Text)
	require.NoError(t, err, "TextContent.Text must be plain base64 with no wrapper")
	assert.Equal(t, testPNGData, decoded)
}

// TestGetPanelImage_TextBase64_RejectsNonPNG verifies that text_base64 refuses
// to silently hand the model bytes for a non-PNG content type. The mode is
// documented as returning PNG bytes; a caller decoding with `base64 -d >
// render.png` would otherwise save a JPEG under a .png filename.
func TestGetPanelImage_TextBase64_RejectsNonPNG(t *testing.T) {
	testJPEGData := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46}
	server := newImageServer(t, testJPEGData, "image/jpeg")
	defer server.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    server.URL,
		APIKey: "test-api-key",
	})

	_, err := getPanelImage(ctx, GetPanelImageParams{
		DashboardUID: "test-dash",
		OutputFormat: stringPtr("text_base64"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image/png")
	assert.Contains(t, err.Error(), "image/jpeg")
}

// TestGetPanelImage_TextBase64_RejectsOversizedPayload verifies that the mode
// refuses payloads large enough to blow past the model's output token window
// on the return trip. Silent truncation there would corrupt the written file
// with no error signal, which is worse than a clean server-side error.
func TestGetPanelImage_TextBase64_RejectsOversizedPayload(t *testing.T) {
	const chunkSize = 64 * 1024
	chunk := bytes.Repeat([]byte{0x89}, chunkSize)

	// Serve textBase64MaxImageBytes+1 bytes starting with PNG magic, so
	// the size guard (not the magic-byte guard) is what trips.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
		remaining := textBase64MaxImageBytes + 1 - 8
		for remaining > 0 {
			n := chunkSize
			if remaining < n {
				n = remaining
			}
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
	}))
	defer server.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    server.URL,
		APIKey: "test-api-key",
	})

	_, err := getPanelImage(ctx, GetPanelImageParams{
		DashboardUID: "test-dash",
		OutputFormat: stringPtr("text_base64"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "text_base64")
	assert.Contains(t, err.Error(), "exceeds")
}

// TestGetPanelImage_TextBase64_RejectsNonPNGBody verifies the magic-byte
// guard fires when upstream lies about the Content-Type. A server serving
// HTML/error pages with a missing or wrong Content-Type would otherwise
// pass the MIME check (parseImageMIME falls back to image/png) and reach
// the model as "PNG bytes".
func TestGetPanelImage_TextBase64_RejectsNonPNGBody(t *testing.T) {
	htmlBody := []byte("<!DOCTYPE html><html><body>not a png</body></html>")
	// Intentionally omit Content-Type so parseImageMIME's fallback kicks
	// in; the magic-byte check must still catch this.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(htmlBody)
	}))
	defer server.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    server.URL,
		APIKey: "test-api-key",
	})

	_, err := getPanelImage(ctx, GetPanelImageParams{
		DashboardUID: "test-dash",
		OutputFormat: stringPtr("text_base64"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a PNG")
}

// TestHasPNGSignature covers the magic-byte helper directly, including the
// nil/short-input edge cases where the byte slice is shorter than the
// 8-byte signature.
func TestHasPNGSignature(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want bool
	}{
		{name: "valid PNG magic", in: []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 'I', 'H', 'D', 'R'}, want: true},
		{name: "exactly 8 bytes of magic", in: []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, want: true},
		{name: "wrong first byte", in: []byte{0x88, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, want: false},
		{name: "HTML", in: []byte("<html>"), want: false},
		{name: "JPEG magic", in: []byte{0xFF, 0xD8, 0xFF, 0xE0}, want: false},
		{name: "too short", in: []byte{0x89, 'P', 'N', 'G'}, want: false},
		{name: "empty", in: []byte{}, want: false},
		{name: "nil", in: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasPNGSignature(tt.in))
		})
	}
}

// TestGetPanelImage_TextBase64_AllowsPayloadAtLimit confirms the size guard
// is "exceeds", not "at or exceeds" — exactly-at-limit renders must succeed.
func TestGetPanelImage_TextBase64_AllowsPayloadAtLimit(t *testing.T) {
	// Prefix with the real PNG magic so the bytes actually look like a
	// PNG — otherwise the magic-byte guard (which runs alongside the
	// size guard) would reject this first and hide a regression in the
	// size check.
	atLimit := make([]byte, textBase64MaxImageBytes)
	copy(atLimit, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	server := newImageServer(t, atLimit, "image/png")
	defer server.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    server.URL,
		APIKey: "test-api-key",
	})

	result, err := getPanelImage(ctx, GetPanelImageParams{
		DashboardUID: "test-dash",
		OutputFormat: stringPtr("text_base64"),
	})
	require.NoError(t, err)
	require.Len(t, imageBlocks(result.Content), 1)
	// Prove the at-limit render round-trips cleanly, not just that
	// an error isn't returned — a regression that swapped TextContent
	// for another shape, or corrupted the base64, would otherwise slip
	// through the size-guard test.
	txt, ok := imageBlocks(result.Content)[0].(*mcp.TextContent)
	require.True(t, ok, "content must be TextContent at size boundary, got %T", imageBlocks(result.Content)[0])
	decoded, err := base64.StdEncoding.DecodeString(txt.Text)
	require.NoError(t, err)
	assert.Equal(t, atLimit, decoded)
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

	res := imageBlocks(result.Content)[0].(*mcp.EmbeddedResource)
	blob := res.Resource
	assert.True(t, strings.HasPrefix(blob.URI, "grafana://render/dashboard/test-dash/panel/42?h="),
		"panel-scoped URI must include dashboard UID, panel ID, and content hash, got %q", blob.URI)
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
		// Gateways like Kong/nginx frequently append parameters; the server
		// must strip them so clients don't see a malformed MIME token.
		{name: "png with charset param", contentType: "image/png; charset=utf-8", wantMIME: "image/png"},
		{name: "png with multiple params", contentType: "image/png; charset=binary; boundary=x", wantMIME: "image/png"},
		{name: "jpeg with spaces and caps", contentType: "IMAGE/JPEG; Quality=85", wantMIME: "image/jpeg"},
		{name: "malformed header falls back", contentType: "image/;;", wantMIME: "image/png"},
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
			require.Len(t, imageBlocks(result.Content), 2)

			img := imageBlocks(result.Content)[0].(*mcp.ImageContent)
			assert.Equal(t, tt.wantMIME, img.MIMEType)

			res := imageBlocks(result.Content)[1].(*mcp.EmbeddedResource)
			blob := res.Resource
			assert.Equal(t, tt.wantMIME, blob.MIMEType)
		})
	}
}

// TestGetPanelImage_ResponseTooLarge covers the OOM guard. Without it, a
// misbehaving upstream that streams multi-GB data would crash the server;
// with it, we refuse to buffer beyond renderResponseLimitBytes and return
// a structured error instead. The handler streams the oversize body in
// small chunks rather than allocating a 25MiB buffer in the test process —
// the client-side LimitReader stops consuming well before the handler
// finishes writing, so only a small prefix is actually transferred.
func TestGetPanelImage_ResponseTooLarge(t *testing.T) {
	const chunkSize = 64 * 1024
	chunk := bytes.Repeat([]byte{0x89}, chunkSize)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		remaining := renderResponseLimitBytes + 1
		for remaining > 0 {
			n := chunkSize
			if remaining < n {
				n = remaining
			}
			if _, err := w.Write(chunk[:n]); err != nil {
				// Client closed the connection — that's the
				// expected path once LimitReader has its bytes.
				return
			}
			remaining -= n
		}
	}))
	defer server.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    server.URL,
		APIKey: "test-api-key",
	})

	_, err := getPanelImage(ctx, GetPanelImageParams{DashboardUID: "test-dash"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum size")
}

// TestGetPanelImage_ErrorBodyBounded verifies the non-200 error path also
// respects renderResponseLimitBytes. A hostile upstream could otherwise
// stream a huge 5xx body to OOM us while we try to include it in the
// error message. Body is streamed in chunks to keep test memory low.
func TestGetPanelImage_ErrorBodyBounded(t *testing.T) {
	const chunkSize = 64 * 1024
	chunk := bytes.Repeat([]byte{'x'}, chunkSize)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		remaining := renderResponseLimitBytes + 1024
		for remaining > 0 {
			n := chunkSize
			if remaining < n {
				n = remaining
			}
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
	}))
	defer server.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    server.URL,
		APIKey: "test-api-key",
	})

	_, err := getPanelImage(ctx, GetPanelImageParams{DashboardUID: "test-dash"})
	require.Error(t, err)
	// The error carries the HTTP code and at most a short prefix of the
	// (potentially multi-megabyte) body: upstream reads 1 KiB of it.
	assert.Contains(t, err.Error(), "HTTP 500")
	assert.Less(t, len(err.Error()), 2048, "oversize error bodies must not leak into the error message")
}

// TestGetPanelImage_EmptyBody covers the 200-OK-with-zero-bytes case. A
// misconfigured image-renderer or a flaky proxy can produce this; we must
// not silently return an empty-base64 ImageContent because downstream
// hooks would then write a zero-byte file and treat the render as
// successful.
func TestGetPanelImage_EmptyBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		// no body
	}))
	defer server.Close()

	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    server.URL,
		APIKey: "test-api-key",
	})

	_, err := getPanelImage(ctx, GetPanelImageParams{DashboardUID: "test-dash"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty image body")
}

// TestParseImageMIME covers the MIME-header normalization function directly,
// including the cases that would otherwise require spinning up httptest
// servers to exercise.
func TestParseImageMIME(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "image/png"},
		{name: "plain png", in: "image/png", want: "image/png"},
		{name: "plain jpeg", in: "image/jpeg", want: "image/jpeg"},
		{name: "png with charset", in: "image/png; charset=utf-8", want: "image/png"},
		{name: "mixed case with params", in: "IMAGE/PNG; Boundary=X", want: "image/png"},
		{name: "non-image", in: "text/html", want: "image/png"},
		{name: "garbled", in: "image/;;", want: "image/png"},
		{name: "application/octet-stream", in: "application/octet-stream", want: "image/png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseImageMIME(tt.in))
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
		{name: "text_base64", input: `{"dashboardUid":"x","outputFormat":"text_base64"}`, want: stringPtr("text_base64")},
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
