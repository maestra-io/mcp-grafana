//go:build unit

package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/mcp-grafana/v2/tools/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuerySQLVariableFormatting(t *testing.T) {
	for _, tc := range []struct {
		name, reference, want string
	}{
		{name: "bare", reference: "$x", want: "O'Reilly"},
		{name: "braced", reference: "${x}", want: "O'Reilly"},
		{name: "sqlstring", reference: "${x:sqlstring}", want: "'O''Reilly'"},
		{name: "bracketed sqlstring", reference: "[[x:sqlstring]]", want: "'O''Reilly'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var submitted string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/datasources/uid/postgres-uid":
					_ = json.NewEncoder(w).Encode(map[string]string{"uid": "postgres-uid", "type": "postgres"})
				case "/api/ds/query":
					var payload map[string]interface{}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
					submitted = safeString(safeArray(payload, "queries")[0].(map[string]interface{}), "rawSql")
					frames := data.Frames{data.NewFrame("", data.NewField("value", nil, []int64{1}))}
					_ = json.NewEncoder(w).Encode(backend.QueryDataResponse{Responses: backend.Responses{"A": {Frames: frames}}})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(ts.Close)
			result, err := querySQLHandler(enforceTestCtx(ts, false), sql.QuerySQLParams{
				DatasourceUID: "postgres-uid",
				Query:         "SELECT " + tc.reference + " LIMIT 1",
				Variables:     map[string]string{"x": "O'Reilly"},
			})
			require.NoError(t, err)
			assert.Equal(t, "SELECT "+tc.want+" LIMIT 1", result.ProcessedQuery)
			assert.Equal(t, result.ProcessedQuery, submitted)
		})
	}
}

// The raw-query tools resolve the datasource type from Grafana by UID and
// refuse anything but their own engine, so an Infinity datasource (which sends
// authenticated HTTP requests) is never reachable through them.
func TestRawQueryToolsRefuseNonSQLDatasource(t *testing.T) {
	var submitted []map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/datasources/uid/inf":
			_ = json.NewEncoder(w).Encode(map[string]string{"uid": "inf", "type": "yesoreyeram-infinity-datasource"})
		case "/api/datasources/uid/ch":
			_ = json.NewEncoder(w).Encode(map[string]string{"uid": "ch", "type": sql.ClickHouseDatasourceType})
		case "/api/ds/query":
			var payload struct {
				Queries []map[string]interface{} `json:"queries"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			submitted = append(submitted, payload.Queries...)
			frames := data.Frames{data.NewFrame("", data.NewField("value", nil, []int64{1}))}
			_ = json.NewEncoder(w).Encode(backend.QueryDataResponse{Responses: backend.Responses{"A": {Frames: frames}}})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	ctx := enforceTestCtx(ts, false)

	_, err := queryClickHouse(ctx, ClickHouseQueryParams{DatasourceUID: "inf", Query: "SELECT 1"})
	assert.ErrorContains(t, err, "unsupported SQL datasource type")
	_, err = querySQLHandler(ctx, sql.QuerySQLParams{DatasourceUID: "inf", Query: "SELECT 1"})
	assert.ErrorContains(t, err, "unsupported SQL datasource type")
	_, err = listClickHouseTables(ctx, ListClickHouseTablesParams{DatasourceUID: "inf"})
	assert.ErrorContains(t, err, "unsupported SQL datasource type")
	_, err = describeClickHouseTable(ctx, DescribeClickHouseTableParams{DatasourceUID: "inf", Table: "t"})
	assert.ErrorContains(t, err, "unsupported SQL datasource type")
	_, err = queryInfluxDB(ctx, InfluxDBQueryParams{DatasourceUID: "inf", Query: "SELECT 1"})
	assert.Error(t, err)
	assert.Empty(t, submitted, "no query may reach /api/ds/query for a non-SQL datasource")

	result, err := queryClickHouse(ctx, ClickHouseQueryParams{DatasourceUID: "ch", Query: "SELECT 1"})
	require.NoError(t, err)
	assert.Equal(t, 1, result.RowCount)
	require.Len(t, submitted, 1)
	assert.Equal(t, map[string]interface{}{"uid": "ch", "type": sql.ClickHouseDatasourceType}, submitted[0]["datasource"])
}
