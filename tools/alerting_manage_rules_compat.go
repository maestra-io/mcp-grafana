package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpgrafana "github.com/grafana/mcp-grafana/v2"
)

// alerting_manage_rules is kept by the maestra-io fork for existing callers
// (MCP gateways assign it by name). Upstream split it into alerting_rules_read
// and alerting_rules_write; this alias takes the fork's old parameters and runs
// their handlers, Grafana-managed rules only, as before.

// ManageRulesReadWriteParams is the old read-write alerting_manage_rules schema.
type ManageRulesReadWriteParams struct {
	listFilterParams

	Operation                   string            `json:"operation" jsonschema:"required,enum=list,enum=get,enum=versions,enum=create,enum=update,enum=delete,description=The operation to perform: 'list'\\, 'get'\\, 'versions'\\, 'create'\\, 'update'\\, or 'delete'. To create a rule\\, use operation 'create' and provide all required fields in a single call. To update a rule\\, first use 'get' to retrieve its full configuration\\, then 'update' with all required fields plus your changes."`
	RuleUID                     string            `json:"rule_uid,omitempty" jsonschema:"description=The UID of the alert rule (required for 'get'\\, 'versions'\\, 'update'\\, 'delete'; optional for 'create')"`
	DatasourceUID               *string           `json:"datasource_uid,omitempty" jsonschema:"description=Optional: UID of a Prometheus or Loki datasource to query for datasource-managed alert rules (for 'list' operation)"`
	Title                       string            `json:"title,omitempty" jsonschema:"description=The title of the alert rule (required for 'create'\\, 'update')"`
	RuleGroup                   string            `json:"rule_group,omitempty" jsonschema:"description=The rule group name (required for 'create'\\, 'update')"`
	FolderUID                   string            `json:"folder_uid,omitempty" jsonschema:"description=The folder UID. For 'list': filter by exact folder UID (mutually exclusive with search_folder). For 'create'/'update': the folder to store the rule in (required)."`
	Condition                   string            `json:"condition,omitempty" jsonschema:"description=The query condition identifier\\, e.g. 'A'\\, 'B' (required for 'create'\\, 'update')"`
	Data                        []map[string]any  `json:"data,omitempty" jsonschema:"description=Array of alert query objects (required for 'create'/'update'). Each object has: datasourceUid (string\\, required)\\, model (object with expr for data queries or type/expression/conditions for expressions)\\, refId (string\\, auto-assigned if omitted)\\, relativeTimeRange ({from\\, to} in seconds\\, defaults to {from:600\\,to:0}). For server-side expressions use datasourceUid '__expr__'."`
	NoDataState                 string            `json:"no_data_state,omitempty" jsonschema:"description=State when no data: NoData\\, Alerting\\, OK (required for 'create'\\, 'update')"`
	ExecErrState                string            `json:"exec_err_state,omitempty" jsonschema:"description=State on execution error: NoData\\, Alerting\\, OK (required for 'create'\\, 'update')"`
	For                         string            `json:"for,omitempty" jsonschema:"description=Duration before alert fires\\, e.g. '5m' (required for 'create'\\, 'update')"`
	KeepFiringFor               string            `json:"keep_firing_for,omitempty" jsonschema:"description=Enables continous firing of alert for specified time even when condition is no longer met. Default is 0 (resolves immediately)"`
	IsPaused                    bool              `json:"is_paused,omitempty" jsonschema:"description=If true\\, the alert rule remains inactive\\, Default is false"`
	NotificationSettings        map[string]any    `json:"notification_settings,omitempty" jsonschema:"description=Notification settings object. Fields: receiver (string\\, required)\\, groupBy ([]string)\\, groupWait/groupInterval/repeatInterval (duration strings)\\, muteTimeIntervals/activeTimeIntervals ([]string)."`
	Record                      map[string]any    `json:"record,omitempty" jsonschema:"description=Recording rule config. Fields: from (string\\, required - ref ID e.g. 'A')\\, metric (string\\, required - metric name)\\, targetDatasourceUid (string\\, optional)."`
	MissingSeriesEvalsToResolve int64             `json:"missing_series_evals_to_resolve,omitempty" jsonschema:"description=Consecutive evaluation intervals with no data required to mark the alert as resolved. Default is 2."`
	Annotations                 map[string]string `json:"annotations,omitempty" jsonschema:"description=Optional annotations for the alert rule"`
	Labels                      map[string]string `json:"labels,omitempty" jsonschema:"description=Optional labels for the alert rule"`
	OrgID                       int64             `json:"org_id,omitempty" jsonschema:"description=The organization ID (required for 'create'\\, 'update')"`
	DisableProvenance           *bool             `json:"disable_provenance,omitempty" jsonschema:"description=If true\\, the alert remains editable in the Grafana UI (sets X-Disable-Provenance header). Defaults to true."`
}

// manageRulesReadWriteCompat routes an old call to the upstream handler. The
// JSON names are shared, so a round trip converts the params.
func manageRulesReadWriteCompat(ctx context.Context, args ManageRulesReadWriteParams) (any, error) {
	isWrite := args.Operation == "create" || args.Operation == "update" || args.Operation == "delete"
	if isWrite {
		// The old tool used datasource_uid only to list; never let it pick a creation target.
		args.DatasourceUID = nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("alerting_manage_rules: %w", err)
	}
	if isWrite {
		var w ManageRulesWriteParams
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, fmt.Errorf("alerting_manage_rules: %w", err)
		}
		return manageRulesWrite(ctx, w)
	}
	var r ManageRulesReadParams
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("alerting_manage_rules: %w", err)
	}
	return manageRulesRead(ctx, r)
}

var ManageRulesReadCompat = mcpgrafana.MustTool(
	"alerting_manage_rules",
	"List and inspect Grafana alert rules (list, get, versions). Kept for existing callers of the old combined rules tool.",
	manageRulesRead,
	mcpgrafana.WithTitleAnnotation("Manage alert rules"),
	mcpgrafana.WithIdempotentHintAnnotation(true),
	mcpgrafana.WithReadOnlyHintAnnotation(true),
	mcpgrafana.WithDestructiveHintAnnotation(false),
	mcpgrafana.WithOpenWorldHintAnnotation(false),
	mcpgrafana.RequiresPermissions("alert.rules:read", "alert.rules.external:read", "folders:read"),
	mcpgrafana.RequiresPermissions("datasources:read"),
)

var ManageRulesReadWriteCompat = mcpgrafana.MustTool(
	"alerting_manage_rules",
	"Manage Grafana-managed alert rules: list, get, versions, create, update, delete. Kept for existing callers of the old combined rules tool.",
	manageRulesReadWriteCompat,
	mcpgrafana.WithTitleAnnotation("Manage alert rules"),
	mcpgrafana.WithReadOnlyHintAnnotation(false),
	mcpgrafana.WithDestructiveHintAnnotation(true),
	mcpgrafana.WithOpenWorldHintAnnotation(false),
	mcpgrafana.RequiresPermissions(
		"alert.rules:read", "alert.rules:create", "alert.rules:write", "alert.rules:delete",
		"alert.provisioning.provenance:write", "folders:read",
		"datasources:read", "alert.rules.external:read", "alert.rules.external:write",
	),
)

// addAlertingCompatTools registers the alias read-write exactly when
// alerting_rules_write is registered, read-only otherwise.
func addAlertingCompatTools(s *mcp.Server, rulesWriteEnabled bool) {
	if rulesWriteEnabled {
		ManageRulesReadWriteCompat.Register(s)
	} else {
		ManageRulesReadCompat.Register(s)
	}
}
