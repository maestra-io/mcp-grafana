package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	mcpgrafana "github.com/grafana/mcp-grafana"
)

// outputFormat* are the wire values accepted on GetPanelImageParams.OutputFormat.
// Kept unexported because they are a JSON wire contract, not a Go API —
// callers pass strings over MCP, not the Go constants.
const (
	outputFormatImage    = "image"    // single ImageContent; legacy/default shape
	outputFormatResource = "resource" // single EmbeddedResource with a BlobResourceContents
	outputFormatBoth     = "both"     // both blocks (image for inline display, resource for out-of-band bytes)
	// outputFormatTextBase64 returns a single TextContent block with just
	// the base64-encoded PNG bytes and nothing else. Motivation: Claude
	// Code surfaces ImageContent (and image/* EmbeddedResource blobs) as
	// images to the user and as vision input to the model, but hides the
	// base64 string from the model's text context — so a skill that
	// wants the model to decode the bytes to disk via Bash/Write has no
	// way to see them. TextContent is passed through as text, which
	// closes that gap. Behavior on non-Claude-Code clients is
	// client-specific: some (Cursor, Zed, Claude Desktop) will render the
	// raw base64 verbatim in the transcript, which is hostile to humans —
	// callers on those clients should stick to 'image'/'resource'/'both'.
	//
	// Cost: the base64 rides back through the model's output window,
	// which means the model must re-emit it (Write tool content or Bash
	// heredoc) to land bytes on disk. Enforced upper bound is
	// textBase64MaxImageBytes; practical limit is lower on models with
	// smaller max_output_tokens. Use only for single-panel screenshots,
	// not full-dashboard captures.
	outputFormatTextBase64 = "text_base64"
	outputFormatDefault    = outputFormatImage
)

// renderResponseLimitBytes caps how much we'll read from the Grafana
// /render endpoint before giving up. Follows the same bounded-read
// pattern as the other HTTP tools in this package (sift, elasticsearch,
// loki, clickhouse — all in the ~10MiB range) but with a higher cap to
// accommodate PNG renders: single panels average ~100KiB and full
// dashboards rarely exceed ~2MiB even at scale=3, so 25MiB leaves
// plenty of headroom while still bounding blast radius.
const renderResponseLimitBytes = 25 * 1024 * 1024 // 25 MiB

// textBase64MaxImageBytes caps the raw-image size accepted by the
// text_base64 output path. The mode requires the model to emit the full
// base64 string in a single output turn (→ Write/Bash), so a too-large
// payload gets silently truncated by the model's output cap and a
// corrupted file lands on disk. Prefer a hard server-side error with a
// clear remediation hint over that silent failure mode.
const textBase64MaxImageBytes = 512 * 1024 // 512 KiB (~683 KiB base64)

// StringOrSlice is a type that can be unmarshaled from either a JSON string
// or an array of strings. This allows dashboard variables to support both
// single-value (e.g., "prometheus") and multi-value (e.g., ["server1", "server2"])
// inputs.
type StringOrSlice []string

// UnmarshalJSON implements the json.Unmarshaler interface.
// It accepts both a JSON string and a JSON array of strings.
func (s *StringOrSlice) UnmarshalJSON(data []byte) error {
	// Try to unmarshal as a single string first.
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = StringOrSlice{single}
		return nil
	}

	// Try to unmarshal as an array of strings.
	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("variables value must be a string or array of strings, got: %s", string(data))
	}
	*s = StringOrSlice(arr)
	return nil
}

// MarshalJSON implements the json.Marshaler interface.
// A single-element slice is marshaled as a plain string for backward compatibility.
func (s StringOrSlice) MarshalJSON() ([]byte, error) {
	if len(s) == 1 {
		return json.Marshal(s[0])
	}
	return json.Marshal([]string(s))
}

// JSONSchema implements the jsonschema.customSchemaGetterType interface so that
// the schema reflector emits a union type: either a string or an array of strings.
func (StringOrSlice) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string"},
			{Type: "array", Items: &jsonschema.Schema{Type: "string"}},
		},
	}
}

type GetPanelImageParams struct {
	DashboardUID string                   `json:"dashboardUid" jsonschema:"required,description=The UID of the dashboard containing the panel"`
	PanelID      *int                     `json:"panelId,omitempty" jsonschema:"description=The ID of the panel to render. If omitted\\, the entire dashboard is rendered"`
	Width        *int                     `json:"width,omitempty" jsonschema:"description=Width of the rendered image in pixels. Defaults to 1000"`
	Height       *int                     `json:"height,omitempty" jsonschema:"description=Height of the rendered image in pixels. Defaults to 500"`
	TimeRange    *RenderTimeRange         `json:"timeRange,omitempty" jsonschema:"description=Time range for the rendered image"`
	Variables    map[string]StringOrSlice `json:"variables,omitempty" jsonschema:"description=Dashboard variables to apply. Values can be a single string or an array of strings for multi-value variables (e.g.\\, {\"var-datasource\": \"prometheus\"\\, \"var-instance\": [\"server1\"\\, \"server2\"]})"`
	Theme        *string                  `json:"theme,omitempty" jsonschema:"description=Theme for the rendered image: light or dark. Defaults to dark"`
	Scale        *int                     `json:"scale,omitempty" jsonschema:"description=Scale factor for the image (1-3). Defaults to 1"`
	Timeout      *int                     `json:"timeout,omitempty" jsonschema:"description=Rendering timeout in seconds. Defaults to 60"`
	OutputFormat *string                  `json:"outputFormat,omitempty" jsonschema:"enum=image,enum=resource,enum=both,enum=text_base64,default=image,description=How to package the rendered bytes. 'image' (default) returns a single MCP ImageContent block for backward compatibility. 'resource' returns an EmbeddedResource with a BlobResourceContents blob - useful for clients or tooling that cannot decode inline ImageContent payloads. 'both' returns both content blocks so the model can display the image inline while out-of-band scripts recover the raw bytes from the resource. 'text_base64' returns a single TextContent block with only the base64 bytes as plain text - use this when the caller needs to pipe the image through a Bash/Write step to land on disk (ImageContent/EmbeddedResource blobs are rendered as images by most MCP clients and are not accessible to the model as text). Note that 'both' roughly doubles response size and 'text_base64' consumes real output tokens."`
}

type RenderTimeRange struct {
	From string `json:"from" jsonschema:"description=Start time (e.g.\\, 'now-1h'\\, '2024-01-01T00:00:00Z')"`
	To   string `json:"to" jsonschema:"description=End time (e.g.\\, 'now'\\, '2024-01-01T12:00:00Z')"`
}

func getPanelImage(ctx context.Context, args GetPanelImageParams) (*mcp.CallToolResult, error) {
	config := mcpgrafana.GrafanaConfigFromContext(ctx)
	baseURL := strings.TrimRight(config.URL, "/")

	if baseURL == "" {
		return nil, fmt.Errorf("grafana URL not configured. Please set GRAFANA_URL environment variable or X-Grafana-URL header")
	}

	// Validate outputFormat up front so we fail fast on typos rather than
	// silently returning the wrong content shape.
	outputFormat, err := resolveOutputFormat(args.OutputFormat)
	if err != nil {
		return nil, err
	}

	// Build the render URL
	renderURL, err := buildRenderURL(baseURL, args)
	if err != nil {
		return nil, fmt.Errorf("failed to build render URL: %w", err)
	}

	// Create HTTP client with TLS configuration if available
	httpClient, err := createHTTPClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP client: %w", err)
	}

	// Set timeout for rendering
	timeout := 60 * time.Second
	if args.Timeout != nil && *args.Timeout > 0 {
		timeout = time.Duration(*args.Timeout) * time.Second
	}
	httpClient.Timeout = timeout

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, renderURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add authentication headers
	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	} else if config.BasicAuth != nil {
		password, _ := config.BasicAuth.Password()
		req.SetBasicAuth(config.BasicAuth.Username(), password)
	}

	// Add org ID header if specified
	if config.OrgID > 0 {
		req.Header.Set("X-Grafana-Org-Id", strconv.FormatInt(config.OrgID, 10))
	}

	// Add user agent
	req.Header.Set("User-Agent", mcpgrafana.UserAgent())

	// Prefer raw image bytes so API gateways (e.g. Kong) that inspect
	// Accept to decide response format return the PNG directly.
	req.Header.Set("Accept", "image/*")

	// Execute request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch panel image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check response status. The error body is bounded by the same limit
	// as the success path so a hostile upstream can't OOM us by streaming
	// a huge 5xx response. If the body overflowed even that cap we skip
	// embedding it in the error message (it would just balloon logs/tool
	// response without adding any useful diagnostic).
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, renderResponseLimitBytes+1))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("image renderer not available. Ensure the Grafana Image Renderer service is installed and configured. See https://grafana.com/docs/grafana/latest/setup-grafana/image-rendering/")
		}
		if int64(len(body)) > renderResponseLimitBytes {
			return nil, fmt.Errorf(
				"failed to render image: HTTP %d - response body exceeded %d-byte limit",
				resp.StatusCode, renderResponseLimitBytes,
			)
		}
		return nil, fmt.Errorf("failed to render image: HTTP %d - %s", resp.StatusCode, string(body))
	}

	// Read the image data. Bounded by renderResponseLimitBytes so a
	// misbehaving upstream can't OOM the server.
	imageData, err := io.ReadAll(io.LimitReader(resp.Body, renderResponseLimitBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}
	if int64(len(imageData)) > renderResponseLimitBytes {
		return nil, fmt.Errorf("render response exceeded %d-byte limit", renderResponseLimitBytes)
	}
	if len(imageData) == 0 {
		// A 200 OK with an empty body would otherwise produce an
		// ImageContent whose Data is "" — downstream hooks then write a
		// zero-byte file and it looks like a successful render. Fail
		// loudly so the caller knows the render actually failed.
		return nil, fmt.Errorf("grafana returned an empty image body (HTTP 200)")
	}

	mimeType := parseImageMIME(resp.Header.Get("Content-Type"))

	// text_base64 is explicitly advertised as "base64 PNG bytes" in the
	// tool description; the model will name the output file .png. Reject
	// non-PNG upstream responses rather than silently emitting bytes a
	// caller will misidentify. We enforce this with two independent
	// checks because neither alone is enough:
	//
	//   1. Content-Type header must be image/png. Catches proxies that
	//      serve a different image format (image/jpeg, image/webp).
	//   2. Magic-byte check on the body itself. Catches upstreams that
	//      serve HTML/error pages with a missing or wrong Content-Type
	//      (parseImageMIME falls back to "image/png" when the header is
	//      absent or malformed, so the first check alone would pass).
	if outputFormat == outputFormatTextBase64 {
		if resp.Header.Get("Content-Type") != "" && mimeType != "image/png" {
			return nil, fmt.Errorf(
				"outputFormat=text_base64 requires Grafana to return image/png; got %q", mimeType,
			)
		}
		if !hasPNGSignature(imageData) {
			return nil, fmt.Errorf(
				"outputFormat=text_base64 requires PNG bytes; response body is not a PNG (got Content-Type %q)",
				resp.Header.Get("Content-Type"),
			)
		}
	}
	// Keep the text_base64 payload small enough that a reasonable Claude
	// output window can actually emit it back to a Write/Bash step. 512KiB
	// of raw PNG → ~683KiB of base64 → ~170K tokens at ~4 chars/token on a
	// base64 alphabet, which is already above a typical 64K output cap
	// but lets the guard catch obvious misuse (full-dashboard renders)
	// without false-positive-ing single-panel screenshots.
	if outputFormat == outputFormatTextBase64 && len(imageData) > textBase64MaxImageBytes {
		return nil, fmt.Errorf(
			"outputFormat=text_base64 payload exceeds %d-byte limit (got %d); reduce width/height/scale or switch to outputFormat=image/resource",
			textBase64MaxImageBytes, len(imageData),
		)
	}

	return buildPanelImageResult(args, outputFormat, mimeType, imageData), nil
}

// pngSignature is the 8-byte ISO/IEC 15948 PNG magic header, as defined
// in https://www.w3.org/TR/PNG/#5PNG-file-signature.
var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// hasPNGSignature reports whether data begins with the PNG magic bytes.
// Used by the text_base64 output path to fail fast on responses that
// pretend to be PNG (via Content-Type) but aren't.
func hasPNGSignature(data []byte) bool {
	return len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature)
}

// parseImageMIME extracts the media type (no params) from a Content-Type
// header, falling back to image/png. It exists because:
//   - Some gateways (Kong/nginx) append parameters like "; charset=utf-8"
//     which are nonsensical for images and confuse strict MCP clients that
//     treat the MIME field as a bare media type.
//   - Completely missing or non-image types (e.g. text/html from an auth
//     wall served as 200 OK) should not be labeled as the real content type —
//     PNG is the only thing the Grafana image renderer actually emits.
func parseImageMIME(raw string) string {
	if raw == "" {
		return "image/png"
	}
	mt, _, err := mime.ParseMediaType(raw)
	if err != nil || !strings.HasPrefix(mt, "image/") {
		return "image/png"
	}
	return mt
}

// resolveOutputFormat validates the caller-supplied outputFormat. A nil
// pointer (field omitted) resolves to the default; every other value must
// match the schema enum exactly. This includes the empty string — an
// explicit "" is a caller bug, not "I don't care", and silently defaulting
// it would hide that. The accepted set is exactly what the JSON schema
// advertises, so a client that validates locally sees the same behavior
// as the server.
func resolveOutputFormat(raw *string) (string, error) {
	if raw == nil {
		return outputFormatDefault, nil
	}
	switch *raw {
	case outputFormatImage, outputFormatResource, outputFormatBoth, outputFormatTextBase64:
		return *raw, nil
	default:
		return "", fmt.Errorf(
			"invalid outputFormat %q: expected one of %q, %q, %q, %q",
			*raw, outputFormatImage, outputFormatResource, outputFormatBoth, outputFormatTextBase64,
		)
	}
}

// ptr is a tiny helper to produce pointer-typed values for MCP annotations,
// which require *float64 to distinguish "not set" from zero.
func ptr[T any](v T) *T { return &v }

// buildPanelImageResult packages raw image bytes into the requested MCP
// content shape. Split out so the HTTP plumbing above stays readable and so
// tests can cover the packaging logic without a live Grafana.
//
// Annotations follow MCP conventions: the ImageContent is marked for both
// the user and the model at high priority (it's the display surface). The
// EmbeddedResource is marked for the user only at low priority so
// context-pruning clients know they can drop it from the model's context
// without losing the image — the out-of-band copy is for tooling, not for
// the LLM.
func buildPanelImageResult(args GetPanelImageParams, outputFormat, mimeType string, imageData []byte) *mcp.CallToolResult {
	base64Data := base64.StdEncoding.EncodeToString(imageData)

	contents := make([]mcp.Content, 0, 2)
	if outputFormat == outputFormatImage || outputFormat == outputFormatBoth {
		contents = append(contents, mcp.ImageContent{
			Annotated: mcp.Annotated{
				Annotations: &mcp.Annotations{
					Audience: []mcp.Role{mcp.RoleUser, mcp.RoleAssistant},
					Priority: ptr(0.9),
				},
			},
			Type:     "image",
			Data:     base64Data,
			MIMEType: mimeType,
		})
	}
	if outputFormat == outputFormatResource || outputFormat == outputFormatBoth {
		contents = append(contents, mcp.EmbeddedResource{
			Annotated: mcp.Annotated{
				Annotations: &mcp.Annotations{
					Audience: []mcp.Role{mcp.RoleUser},
					Priority: ptr(0.1),
				},
			},
			Type: "resource",
			Resource: mcp.BlobResourceContents{
				URI:      buildRenderResourceURI(args, imageData),
				MIMEType: mimeType,
				Blob:     base64Data,
			},
		})
	}
	if outputFormat == outputFormatTextBase64 {
		// Emit ONLY the base64 string, with no surrounding wrapper or
		// prose. The caller is expected to read this verbatim into a
		// Bash/script step (e.g. `base64 -d > file.png`), so any extra
		// characters would corrupt the decoded output.
		contents = append(contents, mcp.TextContent{
			Type: "text",
			Text: base64Data,
		})
	}

	return &mcp.CallToolResult{Content: contents}
}

// buildRenderResourceURI produces an opaque identifier for the
// EmbeddedResource. It is NOT resolvable via resources/read — this server
// does not register a handler for the grafana:// scheme — the URI exists
// purely to let clients/caches distinguish one render from another.
//
// A content-hash suffix is appended so that two renders of the same
// dashboard with different time ranges, variables, or dimensions get
// different URIs (otherwise clients that dedupe EmbeddedResource by URI
// — which is spec-legal — would collapse them into one). Re-rendering
// the exact same state produces the same URI by design, making it
// usable as a weak cache key.
func buildRenderResourceURI(args GetPanelImageParams, imageData []byte) string {
	uid := url.PathEscape(args.DashboardUID)
	sum := sha256.Sum256(imageData)
	// 128 bits (32 hex chars) keeps the collision probability negligible
	// even across millions of distinct renders — a shorter prefix would
	// start aliasing surprisingly quickly (birthday bound on 32 bits is
	// ~65k renders).
	hash := hex.EncodeToString(sum[:16])
	if args.PanelID != nil {
		return fmt.Sprintf("grafana://render/dashboard/%s/panel/%d?h=%s", uid, *args.PanelID, hash)
	}
	return fmt.Sprintf("grafana://render/dashboard/%s?h=%s", uid, hash)
}

func buildRenderURL(baseURL string, args GetPanelImageParams) (string, error) {
	// Strip trailing slashes from base URL for consistent URL construction
	baseURL = strings.TrimRight(baseURL, "/")

	// Build the render path
	renderPath := fmt.Sprintf("/render/d/%s", args.DashboardUID)

	// Build query parameters
	params := url.Values{}

	// Set dimensions
	width := 1000
	height := 500
	if args.Width != nil {
		width = *args.Width
	}
	if args.Height != nil {
		height = *args.Height
	}
	params.Set("width", strconv.Itoa(width))
	params.Set("height", strconv.Itoa(height))

	// Set scale
	scale := 1
	if args.Scale != nil && *args.Scale >= 1 && *args.Scale <= 3 {
		scale = *args.Scale
	}
	params.Set("scale", strconv.Itoa(scale))

	// Add panel ID if specified (for single panel rendering)
	if args.PanelID != nil {
		params.Set("viewPanel", strconv.Itoa(*args.PanelID))
	}

	// Add time range
	if args.TimeRange != nil {
		if args.TimeRange.From != "" {
			params.Set("from", args.TimeRange.From)
		}
		if args.TimeRange.To != "" {
			params.Set("to", args.TimeRange.To)
		}
	}

	// Add theme
	if args.Theme != nil {
		params.Set("theme", *args.Theme)
	}

	// Add dashboard variables (supports multi-value via params.Add)
	for key, values := range args.Variables {
		for _, v := range values {
			params.Add(key, v)
		}
	}

	// Add kiosk mode options for cleaner rendering
	params.Set("kiosk", "true")

	return fmt.Sprintf("%s%s?%s", baseURL, renderPath, params.Encode()), nil
}

func createHTTPClient(config mcpgrafana.GrafanaConfig) (*http.Client, error) {
	transport, err := mcpgrafana.BuildTransport(&config, nil)
	if err != nil {
		return nil, err
	}
	transport = mcpgrafana.NewOrgIDRoundTripper(transport, config.OrgID)
	transport = mcpgrafana.NewUserAgentTransport(transport)

	return &http.Client{Transport: transport}, nil
}

var GetPanelImage = mcpgrafana.MustTool(
	"get_panel_image",
	"Render a Grafana dashboard panel or full dashboard as a PNG image. Returns the image as base64 encoded data. Requires the Grafana Image Renderer service to be installed. Use this for generating visual snapshots of dashboards for reports\\, alerts\\, or presentations.",
	getPanelImage,
	mcp.WithTitleAnnotation("Get panel or dashboard image"),
	mcp.WithIdempotentHintAnnotation(true),
	mcp.WithReadOnlyHintAnnotation(true),
)

func AddRenderingTools(mcp *server.MCPServer) {
	GetPanelImage.Register(mcp)
}
