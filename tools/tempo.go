package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	mcpgrafana "github.com/grafana/mcp-grafana"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// DefaultTempoSearchLimit is the default number of traces returned by a
// query_tempo_traces call when the caller does not specify a limit.
const DefaultTempoSearchLimit = 20

// tempoResponseLimit caps how much of a Tempo API response body we read, to
// avoid loading an unbounded trace into memory.
const tempoResponseLimit = 1024 * 1024 * 10 // 10MB

func AddTempoTools(mcp *server.MCPServer) {
	QueryTempoTraces.Register(mcp)
	GetTempoTrace.Register(mcp)
	ListTempoTagNames.Register(mcp)
	ListTempoTagValues.Register(mcp)
}

// tempoClient talks to a Tempo-compatible HTTP API (Grafana Tempo or
// VictoriaTraces' `/select/tempo` endpoint) through the Grafana datasource
// proxy. It mirrors the Loki client: BuildTransport applies auth/TLS and the
// fallback transport handles /proxy vs /resources deployment differences.
type tempoClient struct {
	httpClient *http.Client
	baseURL    string
}

func newTempoClient(ctx context.Context, uid string) (*tempoClient, error) {
	// First check if the datasource exists.
	if _, err := getDatasourceByUID(ctx, GetDatasourceByUIDParams{UID: uid}); err != nil {
		return nil, err
	}

	cfg := mcpgrafana.GrafanaConfigFromContext(ctx)
	resourcesBase, proxyBase := datasourceProxyPaths(uid)
	baseURL := cfg.URL + proxyBase

	transport, err := mcpgrafana.BuildTransport(&cfg, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create custom transport: %w", err)
	}

	// Wrap with fallback transport: try /proxy first, fall back to /resources
	// on 403/500 for compatibility with different managed Grafana deployments.
	rt := newDatasourceFallbackTransport(transport, proxyBase, resourcesBase)

	return &tempoClient{
		httpClient: &http.Client{Transport: rt},
		baseURL:    baseURL,
	}, nil
}

// get issues a GET against a Tempo API path (relative to the datasource proxy
// base) and returns the raw response body. It always requests JSON so the
// trace-by-id endpoint returns OTLP-JSON rather than protobuf.
func (c *tempoClient) get(ctx context.Context, urlPath string, params url.Values) ([]byte, error) {
	u, err := url.Parse(c.baseURL + urlPath)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}
	if params != nil {
		u.RawQuery = params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close() //nolint:errcheck
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, tempoResponseLimit))
		return nil, fmt.Errorf("tempo API returned status code %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, tempoResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// query_tempo_traces — TraceQL search
// ---------------------------------------------------------------------------

const queryTempoTracesToolPrompt = `
Searches a Tempo (or VictoriaTraces Tempo-compatible) datasource for traces matching a TraceQL query.
Returns a list of matching traces with their trace ID, root service, root span name, start time, and duration in milliseconds.
Use the trace IDs with get_tempo_trace to inspect the full span tree.

TraceQL examples:
- '{}' — all traces (default)
- '{ resource.service.name = "mcp" }' — traces with a span from service "mcp"
- '{ duration > 200ms }' — traces with a span longer than 200ms
- '{ resource.service.name = "mcp" && span.http.status_code >= 500 }' — combine matchers with &&

If the time range is not provided, it defaults to the last hour. Use list_tempo_tag_names / list_tempo_tag_values to discover available attributes.
`

var QueryTempoTraces = mcpgrafana.MustTool(
	"query_tempo_traces",
	queryTempoTracesToolPrompt,
	queryTempoTraces,
	mcp.WithTitleAnnotation("Query Tempo traces"),
	mcp.WithIdempotentHintAnnotation(true),
	mcp.WithReadOnlyHintAnnotation(true),
)

type QueryTempoTracesParams struct {
	DataSourceUID string `json:"data_source_uid" jsonschema:"required,description=The UID of the Tempo datasource to query"`
	Query         string `json:"query,omitempty" jsonschema:"description=A TraceQL query (defaults to {} which matches all traces)"`
	Limit         int    `json:"limit,omitempty" jsonschema:"description=Maximum number of traces to return (default 20)"`
	StartRFC3339  string `json:"start_rfc_3339,omitempty" jsonschema:"description=Optionally\\, the start time of the query in RFC3339 format (defaults to 1 hour ago)"`
	EndRFC3339    string `json:"end_rfc_3339,omitempty" jsonschema:"description=Optionally\\, the end time of the query in RFC3339 format (defaults to now)"`
}

// tempoSearchResponse is the relevant subset of Tempo's /api/search response.
type tempoSearchResponse struct {
	Traces []tempoTraceSummaryRaw `json:"traces"`
}

type tempoTraceSummaryRaw struct {
	TraceID           string   `json:"traceID"`
	RootServiceName   string   `json:"rootServiceName"`
	RootTraceName     string   `json:"rootTraceName"`
	StartTimeUnixNano unixNano `json:"startTimeUnixNano"`
	DurationMs        int64    `json:"durationMs"`
}

// unixNano is a nanosecond Unix timestamp that tolerates being encoded as
// either a JSON number or a JSON string. Grafana Tempo returns it as a string,
// while VictoriaTraces returns it as a number at the trace-summary level (and
// as a string inside spanSets) — so we must accept both.
type unixNano int64

func (u *unixNano) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*u = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid unix-nano timestamp %q: %w", s, err)
	}
	*u = unixNano(n)
	return nil
}

// TempoTraceSummary is the cleaned-up trace summary returned to the caller.
type TempoTraceSummary struct {
	TraceID         string `json:"traceID"`
	RootServiceName string `json:"rootServiceName,omitempty"`
	RootTraceName   string `json:"rootTraceName,omitempty"`
	StartTime       string `json:"startTime,omitempty"`
	DurationMs      int64  `json:"durationMs"`
}

func queryTempoTraces(ctx context.Context, args QueryTempoTracesParams) ([]TempoTraceSummary, error) {
	query := stringOrDefault(args.Query, "{}")

	start, end, err := tempoTimeRange(args.StartRFC3339, args.EndRFC3339)
	if err != nil {
		return nil, err
	}

	limit := args.Limit
	if limit <= 0 {
		limit = DefaultTempoSearchLimit
	}

	client, err := newTempoClient(ctx, args.DataSourceUID)
	if err != nil {
		return nil, fmt.Errorf("failed to create Tempo client: %w", err)
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))
	params.Set("limit", strconv.Itoa(limit))

	body, err := client.get(ctx, "/api/search", params)
	if err != nil {
		return nil, fmt.Errorf("failed to search Tempo: %w", err)
	}

	var resp tempoSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshalling Tempo search response (content: %s): %w", truncateForLog(string(body), 500), err)
	}

	summaries := make([]TempoTraceSummary, 0, len(resp.Traces))
	for _, t := range resp.Traces {
		summaries = append(summaries, TempoTraceSummary{
			TraceID:         t.TraceID,
			RootServiceName: t.RootServiceName,
			RootTraceName:   t.RootTraceName,
			StartTime:       formatUnixNano(int64(t.StartTimeUnixNano)),
			DurationMs:      t.DurationMs,
		})
	}
	return summaries, nil
}

// ---------------------------------------------------------------------------
// get_tempo_trace — fetch a full trace by ID
// ---------------------------------------------------------------------------

const getTempoTraceToolPrompt = `
Fetches a single trace by its trace ID from a Tempo (or VictoriaTraces Tempo-compatible) datasource.
Returns the trace as OTLP-JSON — resource spans with their attributes, status, and per-span timing
(startTimeUnixNano / endTimeUnixNano in nanoseconds). VictoriaTraces' protobuf response is decoded to
JSON automatically. Discover trace IDs first with query_tempo_traces. If the time range is not provided,
it defaults to the last hour.
`

var GetTempoTrace = mcpgrafana.MustTool(
	"get_tempo_trace",
	getTempoTraceToolPrompt,
	getTempoTrace,
	mcp.WithTitleAnnotation("Get Tempo trace"),
	mcp.WithIdempotentHintAnnotation(true),
	mcp.WithReadOnlyHintAnnotation(true),
)

type GetTempoTraceParams struct {
	DataSourceUID string `json:"data_source_uid" jsonschema:"required,description=The UID of the Tempo datasource to query"`
	TraceID       string `json:"trace_id" jsonschema:"required,description=The trace ID to fetch"`
	StartRFC3339  string `json:"start_rfc_3339,omitempty" jsonschema:"description=Optionally\\, the start time of the query in RFC3339 format (defaults to 1 hour ago)"`
	EndRFC3339    string `json:"end_rfc_3339,omitempty" jsonschema:"description=Optionally\\, the end time of the query in RFC3339 format (defaults to now)"`
}

func getTempoTrace(ctx context.Context, args GetTempoTraceParams) (string, error) {
	traceID := strings.TrimSpace(args.TraceID)
	if traceID == "" {
		return "", fmt.Errorf("trace_id is required")
	}

	start, end, err := tempoTimeRange(args.StartRFC3339, args.EndRFC3339)
	if err != nil {
		return "", err
	}

	client, err := newTempoClient(ctx, args.DataSourceUID)
	if err != nil {
		return "", fmt.Errorf("failed to create Tempo client: %w", err)
	}

	params := url.Values{}
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))

	body, err := client.get(ctx, "/api/v2/traces/"+url.PathEscape(traceID), params)
	if err != nil {
		return "", fmt.Errorf("failed to fetch Tempo trace: %w", err)
	}
	return tempoTraceToJSON(body), nil
}

// tempoTraceToJSON normalizes a trace-by-id response into readable JSON.
// Grafana Tempo honors Accept: application/json and already returns JSON, but
// VictoriaTraces ignores it and returns the trace as OTLP protobuf — in which
// the per-span timings and IDs are binary and unreadable. We decode that to
// OTLP-JSON so span durations/attributes are usable. If the body is neither
// JSON nor a decodable OTLP TracesData, it is returned unchanged (no regression).
func tempoTraceToJSON(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return string(body)
	}
	// Already JSON (Grafana Tempo path).
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return string(body)
	}
	// VictoriaTraces path: OTLP protobuf → OTLP-JSON.
	var td tracepb.TracesData
	if err := proto.Unmarshal(body, &td); err == nil && len(td.ResourceSpans) > 0 {
		if js, err := protojson.Marshal(&td); err == nil {
			return string(js)
		}
	}
	return string(body)
}

// ---------------------------------------------------------------------------
// list_tempo_tag_names — discover searchable attributes
// ---------------------------------------------------------------------------

const listTempoTagNamesToolPrompt = `
Lists the searchable tag (attribute) names available in a Tempo (or VictoriaTraces Tempo-compatible) datasource.
Use these names to build TraceQL queries for query_tempo_traces, and list_tempo_tag_values to see the values for a tag.
If the time range is not provided, it defaults to the last hour.
`

var ListTempoTagNames = mcpgrafana.MustTool(
	"list_tempo_tag_names",
	listTempoTagNamesToolPrompt,
	listTempoTagNames,
	mcp.WithTitleAnnotation("List Tempo tag names"),
	mcp.WithIdempotentHintAnnotation(true),
	mcp.WithReadOnlyHintAnnotation(true),
)

type ListTempoTagNamesParams struct {
	DataSourceUID string `json:"data_source_uid" jsonschema:"required,description=The UID of the Tempo datasource to query"`
	StartRFC3339  string `json:"start_rfc_3339,omitempty" jsonschema:"description=Optionally\\, the start time of the query in RFC3339 format (defaults to 1 hour ago)"`
	EndRFC3339    string `json:"end_rfc_3339,omitempty" jsonschema:"description=Optionally\\, the end time of the query in RFC3339 format (defaults to now)"`
}

// tempoTagNamesResponse is the relevant subset of Tempo's /api/v2/search/tags
// response. Tags are grouped under scopes (resource, span, intrinsic).
type tempoTagNamesResponse struct {
	Scopes []struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	} `json:"scopes"`
}

func listTempoTagNames(ctx context.Context, args ListTempoTagNamesParams) ([]string, error) {
	start, end, err := tempoTimeRange(args.StartRFC3339, args.EndRFC3339)
	if err != nil {
		return nil, err
	}

	client, err := newTempoClient(ctx, args.DataSourceUID)
	if err != nil {
		return nil, fmt.Errorf("failed to create Tempo client: %w", err)
	}

	params := url.Values{}
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))

	body, err := client.get(ctx, "/api/v2/search/tags", params)
	if err != nil {
		return nil, fmt.Errorf("failed to list Tempo tag names: %w", err)
	}

	var resp tempoTagNamesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshalling Tempo tag names response (content: %s): %w", truncateForLog(string(body), 500), err)
	}

	// Flatten scopes into a single deduplicated, sorted list of tag names.
	seen := make(map[string]struct{})
	names := make([]string, 0)
	for _, scope := range resp.Scopes {
		for _, tag := range scope.Tags {
			if _, ok := seen[tag]; ok {
				continue
			}
			seen[tag] = struct{}{}
			names = append(names, tag)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ---------------------------------------------------------------------------
// list_tempo_tag_values — discover values for a given attribute
// ---------------------------------------------------------------------------

const listTempoTagValuesToolPrompt = `
Lists the values of a specific tag (attribute) in a Tempo (or VictoriaTraces Tempo-compatible) datasource.
Discover tag names first with list_tempo_tag_names. Use the values to build TraceQL queries for query_tempo_traces.
If the time range is not provided, it defaults to the last hour.
`

var ListTempoTagValues = mcpgrafana.MustTool(
	"list_tempo_tag_values",
	listTempoTagValuesToolPrompt,
	listTempoTagValues,
	mcp.WithTitleAnnotation("List Tempo tag values"),
	mcp.WithIdempotentHintAnnotation(true),
	mcp.WithReadOnlyHintAnnotation(true),
)

type ListTempoTagValuesParams struct {
	DataSourceUID string `json:"data_source_uid" jsonschema:"required,description=The UID of the Tempo datasource to query"`
	Tag           string `json:"tag" jsonschema:"required,description=The tag (attribute) name to list values for. Use a TraceQL-scoped identifier\\, e.g. 'resource.service.name'\\, 'span.http.status_code'\\, or an intrinsic like 'name' or 'status'"`
	StartRFC3339  string `json:"start_rfc_3339,omitempty" jsonschema:"description=Optionally\\, the start time of the query in RFC3339 format (defaults to 1 hour ago)"`
	EndRFC3339    string `json:"end_rfc_3339,omitempty" jsonschema:"description=Optionally\\, the end time of the query in RFC3339 format (defaults to now)"`
}

// tempoTagValuesResponse is the relevant subset of Tempo's
// /api/v2/search/tag/<tag>/values response. Each value carries its type.
type tempoTagValuesResponse struct {
	TagValues []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"tagValues"`
}

func listTempoTagValues(ctx context.Context, args ListTempoTagValuesParams) ([]string, error) {
	tag := strings.TrimSpace(args.Tag)
	if tag == "" {
		return nil, fmt.Errorf("tag is required")
	}

	start, end, err := tempoTimeRange(args.StartRFC3339, args.EndRFC3339)
	if err != nil {
		return nil, err
	}

	client, err := newTempoClient(ctx, args.DataSourceUID)
	if err != nil {
		return nil, fmt.Errorf("failed to create Tempo client: %w", err)
	}

	params := url.Values{}
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))

	body, err := client.get(ctx, "/api/v2/search/tag/"+url.PathEscape(tag)+"/values", params)
	if err != nil {
		return nil, fmt.Errorf("failed to list Tempo tag values: %w", err)
	}

	var resp tempoTagValuesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshalling Tempo tag values response (content: %s): %w", truncateForLog(string(body), 500), err)
	}

	values := make([]string, 0, len(resp.TagValues))
	for _, v := range resp.TagValues {
		values = append(values, v.Value)
	}
	return values, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// tempoTimeRange parses optional RFC3339 start/end strings, defaulting to the
// last hour, and validates the resulting range.
func tempoTimeRange(startRFC3339, endRFC3339 string) (time.Time, time.Time, error) {
	start, err := rfc3339OrDefault(startRFC3339, time.Time{})
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("failed to parse start timestamp %q: %w", startRFC3339, err)
	}
	end, err := rfc3339OrDefault(endRFC3339, time.Time{})
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("failed to parse end timestamp %q: %w", endRFC3339, err)
	}
	return validateTimeRange(start, end)
}

// truncateForLog bounds a string used in an error message so an unexpected
// large response body (e.g. an HTML error page, up to tempoResponseLimit) does
// not flood logs.
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "... (truncated)"
}

// formatUnixNano converts a nanoseconds-since-Unix-epoch timestamp into an
// RFC3339 string. Returns "" for non-positive values.
func formatUnixNano(n int64) string {
	if n <= 0 {
		return ""
	}
	return time.Unix(0, n).UTC().Format(time.RFC3339)
}
