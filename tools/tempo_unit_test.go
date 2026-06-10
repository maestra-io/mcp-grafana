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
	assert.Equal(t, want.Format(time.RFC3339), formatUnixNano("1735689600000000000"))

	assert.Equal(t, "", formatUnixNano(""))
	assert.Equal(t, "", formatUnixNano("   "))
	assert.Equal(t, "", formatUnixNano("0"))
	assert.Equal(t, "", formatUnixNano("not-a-number"))
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
				"startTimeUnixNano": "1735689600000000000",
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
	assert.Equal(t, "2025-01-01T00:00:00Z", formatUnixNano(first.StartTimeUnixNano))

	second := resp.Traces[1]
	assert.Equal(t, "def456", second.TraceID)
	assert.Empty(t, second.RootTraceName)
	assert.Equal(t, "", formatUnixNano(second.StartTimeUnixNano))
}

func TestTempoTagResponseParsing(t *testing.T) {
	var names tempoTagNamesResponse
	require.NoError(t, json.Unmarshal([]byte(`{"tagNames":["service.name","http.status_code"]}`), &names))
	assert.Equal(t, []string{"service.name", "http.status_code"}, names.TagNames)

	var values tempoTagValuesResponse
	require.NoError(t, json.Unmarshal([]byte(`{"tagValues":["mcp","haproxy"]}`), &values))
	assert.Equal(t, []string{"mcp", "haproxy"}, values.TagValues)
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
		_, _ = w.Write([]byte(`{"tagNames":["service.name"]}`))
	}))
	defer srv.Close()

	client := &tempoClient{httpClient: srv.Client(), baseURL: srv.URL}

	t.Run("happy path", func(t *testing.T) {
		params := url.Values{}
		params.Set("start", "100")
		body, err := client.get(context.Background(), "/api/search/tags", params)
		require.NoError(t, err)
		assert.Equal(t, "/api/search/tags", gotPath)
		assert.Equal(t, "application/json", gotAccept)
		assert.Equal(t, "start=100", gotQuery)

		var resp tempoTagNamesResponse
		require.NoError(t, json.Unmarshal(body, &resp))
		assert.Equal(t, []string{"service.name"}, resp.TagNames)
	})

	t.Run("non-200 surfaces status and body", func(t *testing.T) {
		_, err := client.get(context.Background(), "/boom", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
		assert.Contains(t, err.Error(), "kaboom")
	})
}
