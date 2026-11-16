package tools

import (
	"context"

	mcpgrafana "github.com/grafana/mcp-grafana/v2"
	"github.com/grafana/mcp-grafana/v2/tools/sql"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ClickHouse tool names kept by the maestra-io fork for existing callers
// (runbooks and prompts call query_clickhouse / list_clickhouse_tables /
// describe_clickhouse_table). Upstream folded them into the generic SQL tools;
// these aliases take the old parameters and run the same SQL handlers.

type ClickHouseQueryParams struct {
	DatasourceUID string            `json:"datasourceUid" jsonschema:"required,description=The UID of the ClickHouse datasource to query."`
	Query         string            `json:"query" jsonschema:"required,description=Raw SQL query. Supports ClickHouse macros: $__timeFilter(column) for time filtering\\, $__from/$__to for millisecond timestamps\\, $__interval/$__interval_ms for calculated intervals\\, and ${varname} for variable substitution."`
	Start         string            `json:"start,omitempty" jsonschema:"description=Start time for the query. Time formats: 'now-1h'\\, '2026-02-02T19:00:00Z'\\, '1738519200000' (Unix ms). Defaults to 1 hour ago."`
	End           string            `json:"end,omitempty" jsonschema:"description=End time for the query. Time formats: 'now'\\, '2026-02-02T19:00:00Z'\\, '1738519200000' (Unix ms). Defaults to now."`
	Variables     map[string]string `json:"variables,omitempty" jsonschema:"description=Template variable substitutions as key-value pairs. Variables can be referenced as ${varname} or $varname in the query."`
	Limit         int               `json:"limit,omitempty" jsonschema:"description=Maximum number of rows to return. Default: 100\\, Max: 1000. If query doesn't contain LIMIT\\, one will be appended."`
}

type ListClickHouseTablesParams struct {
	DatasourceUID string `json:"datasourceUid" jsonschema:"required,description=The UID of the ClickHouse datasource"`
	Database      string `json:"database,omitempty" jsonschema:"description=Database name to filter tables (lists all non-system databases if not specified)"`
}

type DescribeClickHouseTableParams struct {
	DatasourceUID string `json:"datasourceUid" jsonschema:"required,description=The UID of the ClickHouse datasource"`
	Table         string `json:"table" jsonschema:"required,description=Table name to describe"`
	Database      string `json:"database,omitempty" jsonschema:"description=Database name (defaults to 'default')"`
}

func queryClickHouse(ctx context.Context, args ClickHouseQueryParams) (*sql.SQLQueryResult, error) {
	return querySQLHandler(ctx, sql.QuerySQLParams{
		DatasourceUID: args.DatasourceUID,
		Query:         args.Query,
		Start:         args.Start,
		End:           args.End,
		Variables:     args.Variables,
		Limit:         args.Limit,
	})
}

func listClickHouseTables(ctx context.Context, args ListClickHouseTablesParams) (*sql.ListSQLTablesResult, error) {
	return listSQLTablesHandler(ctx, sql.ListSQLTablesParams{DatasourceUID: args.DatasourceUID, Database: args.Database})
}

func describeClickHouseTable(ctx context.Context, args DescribeClickHouseTableParams) (*sql.DescribeSQLTableResult, error) {
	return describeSQLTableHandler(ctx, sql.DescribeSQLTableParams{DatasourceUID: args.DatasourceUID, Table: args.Table, Database: args.Database})
}

var QueryClickHouse = mcpgrafana.MustTool(
	"query_clickhouse",
	"Query ClickHouse via Grafana with raw SQL. List the tables and describe their columns first. Supports macros: $__timeFilter(column), $__from, $__to, $__interval, ${varname}. Kept for existing callers; same as the generic SQL query tool.",
	queryClickHouse,
	mcpgrafana.WithTitleAnnotation("Query ClickHouse"),
	mcpgrafana.WithIdempotentHintAnnotation(false),
	mcpgrafana.WithReadOnlyHintAnnotation(false),
	mcpgrafana.WithDestructiveHintAnnotation(true),
	mcpgrafana.WithOpenWorldHintAnnotation(false),
	mcpgrafana.RequiresPermissions(datasourceQuery...),
)

var ListClickHouseTables = mcpgrafana.MustTool(
	"list_clickhouse_tables",
	"START HERE for ClickHouse: list available tables (database, name, engine). Kept for existing callers; same as the generic SQL table listing.",
	listClickHouseTables,
	mcpgrafana.WithTitleAnnotation("List ClickHouse tables"),
	mcpgrafana.WithIdempotentHintAnnotation(true),
	mcpgrafana.WithReadOnlyHintAnnotation(true),
	mcpgrafana.WithDestructiveHintAnnotation(false),
	mcpgrafana.WithOpenWorldHintAnnotation(false),
	mcpgrafana.RequiresPermissions(datasourceQuery...),
)

var DescribeClickHouseTable = mcpgrafana.MustTool(
	"describe_clickhouse_table",
	"Get column schema for a ClickHouse table. Pass the database from the table listing. Kept for existing callers; same as the generic SQL table description.",
	describeClickHouseTable,
	mcpgrafana.WithTitleAnnotation("Describe ClickHouse table"),
	mcpgrafana.WithIdempotentHintAnnotation(true),
	mcpgrafana.WithReadOnlyHintAnnotation(true),
	mcpgrafana.WithDestructiveHintAnnotation(false),
	mcpgrafana.WithOpenWorldHintAnnotation(false),
	mcpgrafana.RequiresPermissions(datasourceQuery...),
)

// addClickHouseCompatTools registers the aliases under the same gate as their
// SQL counterparts: query_clickhouse only where query_sql is registered.
func addClickHouseCompatTools(s *mcp.Server, enableQueryTools bool) {
	if enableQueryTools {
		QueryClickHouse.Register(s)
	}
	ListClickHouseTables.Register(s)
	DescribeClickHouseTable.Register(s)
}
