//go:build unit

package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManageRulesCompatSchema: the fork alias keeps its name and old parameters.
func TestManageRulesCompatSchema(t *testing.T) {
	rw := ManageRulesReadWriteCompat.Tool
	ro := ManageRulesReadCompat.Tool
	assert.Equal(t, "alerting_manage_rules", rw.Name)
	assert.Equal(t, "alerting_manage_rules", ro.Name)
	assert.True(t, ro.Annotations.ReadOnlyHint)
	assert.False(t, rw.Annotations.ReadOnlyHint)

	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	raw, err := json.Marshal(rw.InputSchema)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &schema))
	assert.ElementsMatch(t, []string{"list", "get", "versions", "create", "update", "delete"}, schema.Properties["operation"].Enum)
	for _, p := range []string{"rule_uid", "datasource_uid", "folder_uid", "rule_group", "title", "data", "label_selectors", "org_id"} {
		assert.Contains(t, schema.Properties, p)
	}
}

// TestManageRulesCompatRouting: reads reach alerting_rules_read's handler and
// writes alerting_rules_write's, each with its own validation.
func TestManageRulesCompatRouting(t *testing.T) {
	ctx := context.Background()
	_, err := manageRulesReadWriteCompat(ctx, ManageRulesReadWriteParams{Operation: "get"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alerting_rules_read: rule_uid is required for 'get'")

	_, err = manageRulesReadWriteCompat(ctx, ManageRulesReadWriteParams{Operation: "delete"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alerting_rules_write: rule_uid is required for 'delete'")

	_, err = manageRulesReadWriteCompat(ctx, ManageRulesReadWriteParams{Operation: "bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alerting_rules_read: unknown operation")
}
