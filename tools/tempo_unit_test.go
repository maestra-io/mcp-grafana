package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatUnixNano(t *testing.T) {
	// 2025-01-01T00:00:00Z in nanoseconds.
	want := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, want.Format(time.RFC3339), formatUnixNano(1735689600000000000))

	assert.Equal(t, "", formatUnixNano(0))
	assert.Equal(t, "", formatUnixNano(-5))
}

func TestUnixNanoUnmarshal(t *testing.T) {
	// VictoriaTraces encodes startTimeUnixNano as a bare JSON number;
	// Grafana Tempo (and VictoriaTraces inside spanSets) as a quoted string.
	cases := map[string]int64{
		`1735689600000000000`:   1735689600000000000,
		`"1735689600000000000"`: 1735689600000000000,
		`0`:                     0,
		`""`:                    0,
		`null`:                  0,
	}
	for in, want := range cases {
		var u unixNano
		require.NoError(t, json.Unmarshal([]byte(in), &u), "input %s", in)
		assert.Equal(t, want, int64(u), "input %s", in)
	}

	var u unixNano
	require.Error(t, json.Unmarshal([]byte(`"not-a-number"`), &u))
}

func TestTruncateForLog(t *testing.T) {
	assert.Equal(t, "short", truncateForLog("short", 500))
	assert.Equal(t, "ab... (truncated)", truncateForLog("abcdef", 2))
	assert.Equal(t, "abc", truncateForLog("abc", 3)) // exactly at limit, not truncated
}

func TestTempoTimeRange(t *testing.T) {
	t.Run("defaults to last hour", func(t *testing.T) {
		start, end, err := tempoTimeRange("", "")
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now(), end, 5*time.Second)
		assert.InDelta(t, time.Hour.Seconds(), end.Sub(start).Seconds(), 2)
	})

	t.Run("explicit range is honored", func(t *testing.T) {
		start, end, err := tempoTimeRange("2025-01-01T00:00:00Z", "2025-01-01T01:00:00Z")
		require.NoError(t, err)
		assert.Equal(t, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), start.UTC())
		assert.Equal(t, time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC), end.UTC())
	})

	t.Run("rejects start after end", func(t *testing.T) {
		_, _, err := tempoTimeRange("2025-01-01T02:00:00Z", "2025-01-01T01:00:00Z")
		require.Error(t, err)
	})

	t.Run("rejects malformed start", func(t *testing.T) {
		_, _, err := tempoTimeRange("not-a-time", "")
		require.Error(t, err)
	})
}

func TestTempoSearchResponseParsing(t *testing.T) {
	raw := `{
		"traces": [
			{
				"traceID": "abc123",
				"rootServiceName": "mcp",
				"rootTraceName": "POST /mcp",
				"startTimeUnixNano": 1735689600000000000,
				"durationMs": 205
			},
			{
				"traceID": "def456",
				"rootServiceName": "haproxy",
				"durationMs": 12
			}
		]
	}`

	var resp tempoSearchResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	require.Len(t, resp.Traces, 2)

	first := resp.Traces[0]
	assert.Equal(t, "abc123", first.TraceID)
	assert.Equal(t, "mcp", first.RootServiceName)
	assert.Equal(t, "POST /mcp", first.RootTraceName)
	assert.Equal(t, int64(205), first.DurationMs)
	assert.Equal(t, "2025-01-01T00:00:00Z", formatUnixNano(int64(first.StartTimeUnixNano)))

	second := resp.Traces[1]
	assert.Equal(t, "def456", second.TraceID)
	assert.Empty(t, second.RootTraceName)
	assert.Equal(t, "", formatUnixNano(int64(second.StartTimeUnixNano)))
}

func TestTempoTagNamesV2Parsing(t *testing.T) {
	// /api/v2/search/tags groups tags under scopes (resource, span, intrinsic).
	var names tempoTagNamesResponse
	raw := `{"scopes":[
		{"name":"resource","tags":["service.name","k8s.namespace.name"]},
		{"name":"span","tags":["http.status_code"]},
		{"name":"intrinsic","tags":["name","status"]}
	]}`
	require.NoError(t, json.Unmarshal([]byte(raw), &names))
	require.Len(t, names.Scopes, 3)
	assert.Equal(t, "resource", names.Scopes[0].Name)
	assert.Equal(t, []string{"service.name", "k8s.namespace.name"}, names.Scopes[0].Tags)
	assert.Equal(t, []string{"name", "status"}, names.Scopes[2].Tags)
}

func TestTempoTagValuesV2Parsing(t *testing.T) {
	// /api/v2/search/tag/<tag>/values returns typed value objects.
	var values tempoTagValuesResponse
	require.NoError(t, json.Unmarshal([]byte(`{"tagValues":[{"type":"string","value":"mcp"},{"type":"string","value":"haproxy"}]}`), &values))
	require.Len(t, values.TagValues, 2)
	assert.Equal(t, "mcp", values.TagValues[0].Value)
	assert.Equal(t, "string", values.TagValues[0].Type)
	assert.Equal(t, "haproxy", values.TagValues[1].Value)
}

// TestTempoClientGet exercises the HTTP layer of the Tempo client against a
// stub server: correct path, JSON Accept header, query params, and error
// handling on non-200 responses.
func TestTempoClientGet(t *testing.T) {
	var gotPath, gotAccept, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotQuery = r.URL.RawQuery
		if r.URL.Path == "/boom" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("kaboom"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scopes":[{"name":"resource","tags":["service.name"]}]}`))
	}))
	defer srv.Close()

	client := &tempoClient{httpClient: srv.Client(), baseURL: srv.URL}

	t.Run("happy path", func(t *testing.T) {
		params := url.Values{}
		params.Set("start", "100")
		body, err := client.get(context.Background(), "/api/v2/search/tags", params)
		require.NoError(t, err)
		assert.Equal(t, "/api/v2/search/tags", gotPath)
		assert.Equal(t, "application/json", gotAccept)
		assert.Equal(t, "start=100", gotQuery)

		var resp tempoTagNamesResponse
		require.NoError(t, json.Unmarshal(body, &resp))
		require.Len(t, resp.Scopes, 1)
		assert.Equal(t, []string{"service.name"}, resp.Scopes[0].Tags)
	})

	t.Run("non-200 surfaces status and body", func(t *testing.T) {
		_, err := client.get(context.Background(), "/boom", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
		assert.Contains(t, err.Error(), "kaboom")
	})
}
