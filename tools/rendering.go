package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpgrafana "github.com/grafana/mcp-grafana/v2"
)

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

// outputFormat* are the wire values accepted on GetPanelImageParams.OutputFormat
// (maestra-io fork). They are a JSON wire contract, not a Go API.
const (
	outputFormatImage    = "image"    // single ImageContent; upstream/default shape
	outputFormatResource = "resource" // single EmbeddedResource with a blob
	outputFormatBoth     = "both"     // image for inline display + resource for out-of-band bytes
	// outputFormatTextBase64 returns a single TextContent block with only the
	// base64-encoded PNG. Claude Code shows ImageContent to the model as vision
	// input but hides the base64, so a skill that must write the PNG to disk
	// (Bash/Write) cannot see the bytes; TextContent closes that gap. The bytes
	// ride through the model's output window, hence textBase64MaxImageBytes.
	outputFormatTextBase64 = "text_base64"
	outputFormatDefault    = outputFormatImage
)

// renderResponseLimitBytes caps how much is read from /render. PNG renders are
// ~100KiB per panel and rarely above ~2MiB for a full dashboard at scale=3.
const renderResponseLimitBytes = 25 * 1024 * 1024 // 25 MiB

// textBase64MaxImageBytes caps the raw image accepted by text_base64: the model
// must re-emit the whole base64 string in one output turn, and a payload cut by
// its output cap would land a corrupt file on disk. Fail loudly instead.
const textBase64MaxImageBytes = 512 * 1024 // 512 KiB (~683 KiB base64)

type GetPanelImageParams struct {
	DashboardUID string                   `json:"dashboardUid,omitempty" jsonschema:"description=The UID of a stored dashboard containing the panel. Required unless provisioningPreview is provided."`
	PanelID      *int                     `json:"panelId,omitempty" jsonschema:"description=The ID of the panel to render. If omitted\\, the entire dashboard is rendered"`
	Width        *int                     `json:"width,omitempty" jsonschema:"description=Positive viewport width in CSS pixels. See tool description for per-view defaults. Capped so width times scale is at most 3000. The renderer may raise values below its configured minimum (1000 by default)."`
	Height       *int                     `json:"height,omitempty" jsonschema:"description=Positive viewport height in CSS pixels\\, or -1 for full-page rendering. See tool description for per-view defaults. Positive heights are capped so height times scale is at most 3000. The renderer may raise values below its configured minimum (500 by default)."`
	TimeRange    *RenderTimeRange         `json:"timeRange,omitempty" jsonschema:"description=Time range for the rendered image"`
	Variables    map[string]StringOrSlice `json:"variables,omitempty" jsonschema:"description=Dashboard variables to apply. Values can be a single string or an array of strings for multi-value variables (e.g.\\, {\"var-datasource\": \"prometheus\"\\, \"var-instance\": [\"server1\"\\, \"server2\"]})"`
	Theme        *string                  `json:"theme,omitempty" jsonschema:"description=Theme for the rendered image: light or dark. Defaults to dark"`
	Scale        *int                     `json:"scale,omitempty" jsonschema:"description=Image scale factor. See tool description for default and limits. Higher scales reduce the maximum allowed viewport dimensions."`
	Timeout      *int                     `json:"timeout,omitempty" jsonschema:"description=Rendering timeout in seconds. Defaults to 60\\, capped at 120"`
	OutputFormat *string                  `json:"outputFormat,omitempty" jsonschema:"enum=image,enum=resource,enum=both,enum=text_base64,default=image,description=How to package the rendered bytes. 'image' (default) returns a single MCP ImageContent block. 'resource' returns an EmbeddedResource with a blob - useful for clients or tooling that cannot decode inline ImageContent payloads. 'both' returns both blocks so the model can display the image inline while out-of-band scripts recover the raw bytes from the resource. 'text_base64' returns a single TextContent block with only the base64 PNG bytes as plain text - use this when the caller needs to pipe the image through a Bash/Write step to land on disk (limit 512 KiB of PNG). Note that 'both' roughly doubles response size and 'text_base64' consumes real output tokens."`
	// ProvisioningPreview renders a dashboard from a provisioning repository
	// branch that has not yet been merged or applied. Mutually exclusive with
	// dashboardUid.
	ProvisioningPreview *ProvisioningPreview `json:"provisioningPreview,omitempty" jsonschema:"description=Render a dashboard from a provisioning repository branch (e.g. a git-sync PR preview). Mutually exclusive with dashboardUid."`
	// Explore renders an ad-hoc Explore view instead of a stored dashboard.
	// Mutually exclusive with dashboardUid and provisioningPreview.
	Explore *RenderExplore `json:"explore,omitempty" jsonschema:"description=Render an ad-hoc Explore view for a datasource and queries instead of a stored dashboard. Mutually exclusive with dashboardUid and provisioningPreview. Requires Grafana 10.2 or later."`
}

// RenderExplore describes an Explore view to render. The state is encoded the
// same way as the explore links produced by generate_deeplink; the time range
// comes from GetPanelImageParams.TimeRange.
type RenderExplore struct {
	DatasourceUID string                   `json:"datasourceUid" jsonschema:"required,description=Datasource UID to query"`
	Queries       []map[string]interface{} `json:"queries,omitempty" jsonschema:"description=List of query objects (e.g. [{\"refId\":\"A\"\\, \"expr\":\"up\"}])"`
}

type RenderTimeRange struct {
	From string `json:"from" jsonschema:"description=Start time (e.g.\\, 'now-1h'\\, '2024-01-01T00:00:00Z')"`
	To   string `json:"to" jsonschema:"description=End time (e.g.\\, 'now'\\, '2024-01-01T12:00:00Z')"`
}

// ProvisioningPreview identifies a not-yet-applied dashboard inside a
// provisioning repository, e.g. one staged on a feature branch via git-sync.
// The renderer loads the dashboard at /dashboard/provisioning/<repo>/preview/<path>
// instead of looking it up by UID.
type ProvisioningPreview struct {
	Repo string `json:"repo" jsonschema:"required,description=Provisioning repository slug. List repositories via /apis/provisioning.grafana.app/v0alpha1/namespaces/default/repositories if unknown."`
	Path string `json:"path" jsonschema:"required,description=Path to the dashboard file within the repository\\, relative to its root."`
	Ref  string `json:"ref,omitempty" jsonschema:"description=Branch or commit SHA to render from. Defaults to the repository's main branch when omitted."`
}

func getPanelImage(ctx context.Context, args GetPanelImageParams) (*mcp.CallToolResult, error) {
	config := mcpgrafana.GrafanaConfigFromContext(ctx)
	baseURL := config.URL

	if baseURL == "" {
		return nil, fmt.Errorf("grafana URL not configured. Please set GRAFANA_URL environment variable")
	}

	// Validate outputFormat up front so a typo fails fast instead of
	// returning the wrong content shape.
	outputFormat, err := resolveOutputFormat(args.OutputFormat)
	if err != nil {
		return nil, err
	}

	// The org to render from comes from config.OrgID, which
	// OrgIDOverrideMiddleware sets from the per-call orgId argument when dynamic
	// multi-org is enabled and which otherwise carries the connection's org. This
	// tool declares no orgId of its own, so it behaves like every other tool: the
	// argument exists only when --dynamic-multi-org is set. Reading the org from
	// the context also keeps the X-Grafana-Org-Id header the transport adds in
	// step with the render URL's targetOrgId.
	renderOrg := config.OrgID

	renderURL, err := buildRenderURL(baseURL, renderOrg, args)
	if err != nil {
		return nil, fmt.Errorf("failed to build render URL: %w", err)
	}

	// Build the HTTP transport using the shared middleware chain (TLS, auth,
	// extra headers, org ID, user-agent, otel). Using config.HTTPTransport()
	// as the base ensures we respect BaseTransport when set (e.g. the hosted
	// Cloud MCP server injects a pre-configured transport with custom
	// instrumentation).
	transport, err := mcpgrafana.BuildTransport(&config, config.HTTPTransport())
	if err != nil {
		return nil, fmt.Errorf("failed to build HTTP transport: %w", err)
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   renderTimeout(args),
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, renderURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Prefer raw image bytes so API gateways (e.g. Kong) that inspect
	// Accept to decide response format return the PNG directly.
	req.Header.Set("Accept", "image/*")

	// Execute request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch panel image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("image renderer not available. Ensure the Grafana Image Renderer service is installed and configured. See https://grafana.com/docs/grafana/latest/setup-grafana/image-rendering/")
		}
		return nil, fmt.Errorf("failed to render image: HTTP %d - %s", resp.StatusCode, string(body))
	}

	// Read the image data
	imageData, err := readResponseBody(resp.Body, renderResponseLimitBytes)
	if err != nil {
		if errors.Is(err, errResponseBodyTooLarge) {
			err = fmt.Errorf("%w of %d bytes; try a smaller width, height or scale", errResponseBodyTooLarge, renderResponseLimitBytes)
		}
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}
	if len(imageData) == 0 {
		// A 200 with an empty body would otherwise look like a successful
		// render and land a zero-byte file downstream.
		return nil, fmt.Errorf("grafana returned an empty image body (HTTP 200)")
	}

	mimeType := parseImageMIME(resp.Header.Get("Content-Type"))
	if outputFormat == outputFormatTextBase64 {
		// text_base64 promises PNG bytes. Check both the header (a proxy
		// serving another image type) and the magic bytes (an HTML error
		// page with a missing or wrong Content-Type).
		if resp.Header.Get("Content-Type") != "" && mimeType != "image/png" {
			return nil, fmt.Errorf("outputFormat=text_base64 requires Grafana to return image/png; got %q", mimeType)
		}
		if !hasPNGSignature(imageData) {
			return nil, fmt.Errorf("outputFormat=text_base64 requires PNG bytes; response body is not a PNG (got Content-Type %q)", resp.Header.Get("Content-Type"))
		}
		if len(imageData) > textBase64MaxImageBytes {
			return nil, fmt.Errorf("outputFormat=text_base64 payload exceeds %d-byte limit (got %d); reduce width/height/scale or switch to outputFormat=image/resource", textBase64MaxImageBytes, len(imageData))
		}
		// Only the base64 string: the caller decodes it verbatim, so no
		// deeplink block or structured content is added.
		return buildPanelImageResult(args, outputFormat, mimeType, imageData), nil
	}

	// Rendering succeeded, so always return the image. The deeplink is
	// best-effort metadata added only if it builds successfully.
	result := buildPanelImageResult(args, outputFormat, mimeType, imageData)
	content := result.Content

	// Use the public base URL, not config.URL, which may be an in-cluster
	// endpoint the browser can't reach.
	var structured any
	if deeplinkBase, err := grafanaBaseURLFromContext(ctx); err == nil && deeplinkResolvesInRenderOrg(ctx, renderOrg) {
		if deeplink, err := buildDashboardDeeplink(deeplinkBase, args); err == nil {
			content = append(content, &mcp.TextContent{
				Meta: mcpgrafana.NewUIContentMeta(mcpgrafana.UIContentKindDeeplink),
				Text: deeplink,
			})
			// Also in structuredContent: some hosts (e.g. Claude) don't forward
			// content items' _meta to the MCP App, so the viewer can't find the
			// link in content alone.
			structured = map[string]any{"deeplink": deeplink}
		}
	}

	return &mcp.CallToolResult{
		Content:           content,
		StructuredContent: structured,
	}, nil
}

// pngSignature is the 8-byte PNG magic header (https://www.w3.org/TR/PNG/#5PNG-file-signature).
var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

func hasPNGSignature(data []byte) bool {
	return len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature)
}

// parseImageMIME extracts the bare media type from a Content-Type header,
// falling back to image/png: gateways append parameters (strict MCP clients
// want a bare type) and a non-image type (an auth wall served as 200) must not
// label the content.
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

// resolveOutputFormat validates outputFormat. Omitted means the default; any
// other value, including "", must match the schema enum exactly.
func resolveOutputFormat(raw *string) (string, error) {
	if raw == nil {
		return outputFormatDefault, nil
	}
	switch *raw {
	case outputFormatImage, outputFormatResource, outputFormatBoth, outputFormatTextBase64:
		return *raw, nil
	default:
		return "", fmt.Errorf("invalid outputFormat %q: expected one of %q, %q, %q, %q",
			*raw, outputFormatImage, outputFormatResource, outputFormatBoth, outputFormatTextBase64)
	}
}

// buildPanelImageResult packages the image bytes into the requested content
// shape. The ImageContent targets user and model at high priority; the
// EmbeddedResource is for tooling (user only, low priority) so
// context-pruning clients may drop it from the model's context.
func buildPanelImageResult(args GetPanelImageParams, outputFormat, mimeType string, imageData []byte) *mcp.CallToolResult {
	contents := make([]mcp.Content, 0, 2)
	if outputFormat == outputFormatImage || outputFormat == outputFormatBoth {
		contents = append(contents, &mcp.ImageContent{
			Annotations: &mcp.Annotations{Audience: []mcp.Role{"user", "assistant"}, Priority: 0.9},
			Data:        imageData, // the SDK base64-encodes on the wire
			MIMEType:    mimeType,
		})
	}
	if outputFormat == outputFormatResource || outputFormat == outputFormatBoth {
		contents = append(contents, &mcp.EmbeddedResource{
			Annotations: &mcp.Annotations{Audience: []mcp.Role{"user"}, Priority: 0.1},
			Resource: &mcp.ResourceContents{
				URI:      buildRenderResourceURI(args, imageData),
				MIMEType: mimeType,
				Blob:     imageData,
			},
		})
	}
	if outputFormat == outputFormatTextBase64 {
		contents = append(contents, &mcp.TextContent{Text: base64.StdEncoding.EncodeToString(imageData)})
	}
	return &mcp.CallToolResult{Content: contents}
}

// buildRenderResourceURI returns an opaque, non-resolvable identifier for the
// EmbeddedResource. The content-hash suffix keeps renders of different states
// apart for clients that dedupe resources by URI.
func buildRenderResourceURI(args GetPanelImageParams, imageData []byte) string {
	uid := url.PathEscape(args.DashboardUID)
	sum := sha256.Sum256(imageData)
	hash := hex.EncodeToString(sum[:16])
	if args.PanelID != nil {
		return fmt.Sprintf("grafana://render/dashboard/%s/panel/%d?h=%s", uid, *args.PanelID, hash)
	}
	return fmt.Sprintf("grafana://render/dashboard/%s?h=%s", uid, hash)
}

// Upper bounds for caller-supplied render parameters. The dimension cap matches
// the image renderer's default maximum viewport size.
const (
	defaultRenderTimeout   = 60 * time.Second
	maxRenderTimeoutSecs   = 120
	maxRenderDimension     = 3000
	defaultPanelWidth      = 1000
	defaultPanelHeight     = 750
	defaultDashboardWidth  = 1200
	defaultDashboardHeight = 900
	defaultExploreWidth    = 1000
	defaultExploreHeight   = 1000
	defaultRenderScale     = 1
	maxRenderScale         = 3
)

func renderTimeout(args GetPanelImageParams) time.Duration {
	if args.Timeout == nil || *args.Timeout <= 0 {
		return defaultRenderTimeout
	}
	// Clamp before converting so large values cannot overflow the Duration.
	return time.Duration(min(*args.Timeout, maxRenderTimeoutSecs)) * time.Second
}

func buildRenderURL(baseURL string, orgID int64, args GetPanelImageParams) (string, error) {
	// Validate that exactly one source is set.
	hasUID := args.DashboardUID != ""
	hasPreview := args.ProvisioningPreview != nil
	hasExplore := args.Explore != nil
	sources := 0
	for _, set := range []bool{hasUID, hasPreview, hasExplore} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		if sources > 1 {
			return "", fmt.Errorf("dashboardUid, provisioningPreview and explore are mutually exclusive; pass exactly one")
		}
		return "", fmt.Errorf("one of dashboardUid, provisioningPreview or explore must be set")
	}
	if hasExplore {
		if args.Explore.DatasourceUID == "" {
			return "", fmt.Errorf("explore.datasourceUid is required")
		}
		if args.PanelID != nil {
			return "", fmt.Errorf("panelId cannot be used with explore")
		}
	}
	if hasPreview {
		if err := validateRepoSlug("provisioningPreview.repo", args.ProvisioningPreview.Repo); err != nil {
			return "", err
		}
		if err := validateRepoPath("provisioningPreview.path", args.ProvisioningPreview.Path); err != nil {
			return "", err
		}
	}

	// Strip trailing slashes from base URL for consistent URL construction
	baseURL = strings.TrimRight(baseURL, "/")

	// Build query parameters
	params := url.Values{}

	// Choose render path. For stored dashboards we use the purpose-built /d-solo
	// route for single-panel renders (lighter than loading the full dashboard
	// with viewPanel); full dashboard renders use /d. For provisioning previews
	// the same route is used for both since the preview UI handles ?panelId
	// via the standard kiosk/viewPanel mechanism.
	var renderPath string
	if hasExplore {
		// The renderer loads /explore in a headless browser, so the state is
		// encoded exactly as in an explore deeplink.
		renderPath = "/render/explore"
		exploreParams, err := buildExploreRenderParams(args)
		if err != nil {
			return "", err
		}
		for key, values := range exploreParams {
			for _, v := range values {
				params.Add(key, v)
			}
		}
		// Newer renderers wait for a "render done" signal that only dashboards
		// send (behind Grafana's reportRenderBinding flag), so an Explore render
		// would hang until it times out. Ask them to poll instead; older
		// renderers ignore the param.
		params.Set("forcePollingMode", "true")
	} else if hasPreview {
		// Repo is a single segment and gets the stricter url.PathEscape (which
		// also encodes sub-delim characters like @, $, &, ;, =, :). For the
		// multi-segment file path we use url.URL.EscapedPath() so structural /
		// separators between segments are preserved while everything else that
		// isn't valid in a URL path is percent-encoded. This matches the
		// encoding done by tools/navigation.go and tools/provisioning.go.
		// Note: we build the path by string-concatenation rather than via
		// `url.URL{Path: ...}` because url.URL would re-escape our PathEscape
		// output (turning %40 into %2540).
		escapedFile := (&url.URL{Path: strings.TrimLeft(args.ProvisioningPreview.Path, "/")}).EscapedPath()
		renderPath = fmt.Sprintf(
			"/render/dashboard/provisioning/%s/preview/%s",
			url.PathEscape(args.ProvisioningPreview.Repo),
			escapedFile,
		)
		if args.ProvisioningPreview.Ref != "" {
			params.Set("ref", args.ProvisioningPreview.Ref)
		}
		if args.PanelID != nil {
			params.Set("viewPanel", strconv.Itoa(*args.PanelID))
		}
	} else {
		renderPath = fmt.Sprintf("/render/d/%s", args.DashboardUID)
		if args.PanelID != nil {
			renderPath = fmt.Sprintf("/render/d-solo/%s", args.DashboardUID)
			params.Set("panelId", strconv.Itoa(*args.PanelID))
		}
	}
	// Choose defaults for the view, then apply each caller override independently.
	width, height, scale := defaultDashboardWidth, defaultDashboardHeight, defaultRenderScale
	if hasExplore {
		// Explore shows its query editor above the graph and scrolls inside
		// its own container (so height=-1 doesn't help); give it room.
		width, height = defaultExploreWidth, defaultExploreHeight
	} else if args.PanelID != nil {
		width, height = defaultPanelWidth, defaultPanelHeight
	}
	if args.Width != nil {
		if *args.Width <= 0 {
			return "", fmt.Errorf("width must be positive")
		}
		width = *args.Width
	}
	if args.Height != nil {
		if *args.Height <= 0 && *args.Height != -1 {
			return "", fmt.Errorf("height must be positive or -1 for full-page rendering")
		}
		height = *args.Height
	}
	if args.Scale != nil {
		scale = min(max(*args.Scale, defaultRenderScale), maxRenderScale)
	}
	// Bound device pixels, not just CSS pixels. Full-page height is determined
	// by the dashboard, so the response byte limit remains necessary too.
	width = min(width, maxRenderDimension/scale)
	if height != -1 {
		height = min(height, maxRenderDimension/scale)
	}
	params.Set("width", strconv.Itoa(width))
	params.Set("height", strconv.Itoa(height))

	params.Set("scale", strconv.Itoa(scale))

	// Add time range. Explore carries it inside the pane state instead.
	if args.TimeRange != nil && !hasExplore {
		if args.TimeRange.From != "" {
			params.Set("from", toGrafanaTimeParam(args.TimeRange.From, false))
		}
		if args.TimeRange.To != "" {
			params.Set("to", toGrafanaTimeParam(args.TimeRange.To, true))
		}
	}

	// Add theme
	if args.Theme != nil {
		params.Set("theme", *args.Theme)
	}

	// Add dashboard variables (supports multi-value via params.Add)
	if !hasExplore {
		for key, values := range args.Variables {
			for _, v := range values {
				params.Add(key, v)
			}
		}
	}

	// Select the org to render from. The image renderer drives a headless browser
	// whose request org is set by the backend from the targetOrgId query param;
	// the X-Grafana-Org-Id header (set by the transport) is ignored by the render
	// path. We deliberately use targetOrgId rather than orgId: orgId is read by
	// the frontend, which persists an org switch via /api/user/using — that would
	// change the user's active org for every later request. targetOrgId only
	// scopes this single render.
	if orgID > 0 {
		params.Set("targetOrgId", strconv.FormatInt(orgID, 10))
	}

	// Add kiosk mode options for cleaner rendering
	params.Set("kiosk", "true")

	return fmt.Sprintf("%s%s?%s", baseURL, renderPath, params.Encode()), nil
}

// buildExploreRenderParams returns the Explore URL state for an Explore render
// or its deeplink, reusing the pane builder behind generate_deeplink.
func buildExploreRenderParams(args GetPanelImageParams) (url.Values, error) {
	var tr *TimeRange
	if args.TimeRange != nil {
		tr = &TimeRange{From: args.TimeRange.From, To: args.TimeRange.To}
	}
	uid := args.Explore.DatasourceUID
	return buildExplorePanesParams(GenerateDeeplinkParams{
		ResourceType:  "explore",
		DatasourceUID: &uid,
		Queries:       args.Explore.Queries,
		TimeRange:     tr,
	})
}

// deeplinkResolvesInRenderOrg reports whether a dashboard URL carrying no org
// would open in renderOrg for this identity.
//
// The link cannot carry one: `?orgId=N` is intercepted by Grafana's global
// OrgRedirect middleware, which persists the switch onto the user's record, and
// neither targetOrgId (render backend only) nor X-Grafana-Org-Id (not sendable
// by a browser) substitutes. So when the render org is not where the viewer would
// land, omit the link rather than point it at the wrong dashboard — UIDs are
// unique only within an org. The image is unaffected.
//
// A non-positive renderOrg means no org reached the render (as in buildRenderURL
// and OrgIDRoundTripper), so it used the identity's own org and the link agrees,
// checked without a request. Any other org needs the lookup, whether it came
// from a per-call orgId or from the connection; a failed lookup omits the link.
func deeplinkResolvesInRenderOrg(ctx context.Context, renderOrg int64) bool {
	if renderOrg <= 0 {
		return true
	}
	viewerOrg, err := mcpgrafana.UserPersistedOrgID(ctx)
	if err != nil {
		return false
	}
	return viewerOrg == renderOrg
}

// buildDashboardDeeplink returns the Grafana UI URL for the rendered
// dashboard, mirroring buildRenderURL's view (panel, time range, variables)
// so the deeplink matches the PNG.
func buildDashboardDeeplink(baseURL string, args GetPanelImageParams) (string, error) {
	baseURL = strings.TrimRight(baseURL, "/")

	if args.Explore != nil {
		params, err := buildExploreRenderParams(args)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s/explore?%s", baseURL, params.Encode()), nil
	}

	var uid *string
	if args.DashboardUID != "" {
		uid = &args.DashboardUID
	}
	var preview *DeeplinkProvisioningPreview
	if args.ProvisioningPreview != nil {
		preview = &DeeplinkProvisioningPreview{
			Repo: args.ProvisioningPreview.Repo,
			Path: args.ProvisioningPreview.Path,
			Ref:  args.ProvisioningPreview.Ref,
		}
	}

	target, err := buildDashboardTargetURL(baseURL, uid, preview)
	if err != nil {
		return "", err
	}

	extra := url.Values{}
	if args.PanelID != nil {
		extra.Set("viewPanel", strconv.Itoa(*args.PanelID))
	}
	if args.TimeRange != nil {
		// Normalize RFC 3339 to epoch ms for Grafana's Scenes URL parser.
		if args.TimeRange.From != "" {
			extra.Set("from", toGrafanaTimeParam(args.TimeRange.From, false))
		}
		if args.TimeRange.To != "" {
			extra.Set("to", toGrafanaTimeParam(args.TimeRange.To, true))
		}
	}
	for key, values := range args.Variables {
		for _, v := range values {
			extra.Add(key, v)
		}
	}

	if len(extra) == 0 {
		return target, nil
	}
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	return target + separator + extra.Encode(), nil
}

var GetPanelImage = mcpgrafana.MustTool(
	"get_panel_image",
	"Render a Grafana dashboard panel, full dashboard or Explore view as a PNG image. Returns the image as base64 encoded data; outputFormat selects image (default), resource, both, or text_base64 (plain-text base64 for writing the PNG to disk). Requires the Grafana Image Renderer service to be installed. "+
		"Exactly one of dashboardUid (for stored dashboards), provisioningPreview (for dashboards staged on a provisioning repository branch, e.g. a git-sync PR) or explore (an ad-hoc datasource query rendered in Explore, no saved dashboard needed) must be supplied. "+
		fmt.Sprintf("Defaults in CSS pixels: panel %dx%d, dashboard %dx%d, Explore %dx%d. Scale defaults to %d for all views and is clamped to %d-%d. Use this for generating visual snapshots of dashboards for reports, alerts, or presentations.",
			defaultPanelWidth, defaultPanelHeight, defaultDashboardWidth, defaultDashboardHeight, defaultExploreWidth, defaultExploreHeight, defaultRenderScale, defaultRenderScale, maxRenderScale),
	getPanelImage,
	mcpgrafana.WithTitleAnnotation("Get panel or dashboard image"),
	mcpgrafana.WithIdempotentHintAnnotation(true),
	mcpgrafana.WithUIResource(mcpgrafana.PanelViewerResourceURI),
	mcpgrafana.WithReadOnlyHintAnnotation(true),
	mcpgrafana.WithDestructiveHintAnnotation(false),
	mcpgrafana.WithOpenWorldHintAnnotation(false),
	mcpgrafana.RequiresPermissions("dashboards:read"),
	mcpgrafana.RequiresPermissions(datasourceQuery...),
	// explore renders /explore; provisioningPreview renders a repository preview.
	mcpgrafana.RequiresPermissions("datasources:explore", "provisioning.repositories:read"),
)

func AddRenderingTools(s *mcp.Server, appTools ...mcpgrafana.Tool) {
	GetPanelImage.Register(s)
	AddAppTool(s, appTools...)
}
