//go:build unit
// +build unit

package mcpgrafana

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type embeddedTimeParams struct {
	StartRFC3339 string `json:"start_rfc_3339,omitempty"`
	EndRFC3339   string `json:"end_rfc_3339,omitempty"`
}

type unknownArgsParams struct {
	embeddedTimeParams
	DataSourceUID string `json:"data_source_uid" jsonschema:"required"`
	Hidden        string `json:"-"`
}

func unknownArgsHandler(ctx context.Context, params unknownArgsParams) (string, error) {
	return "ok", nil
}

func TestUnknownArguments(t *testing.T) {
	properties := map[string]any{"data_source_uid": nil, "start_rfc_3339": nil}

	t.Run("accepts exact and case-insensitive keys", func(t *testing.T) {
		args := map[string]any{"data_source_uid": "x", "START_RFC_3339": "t"}
		assert.Empty(t, unknownArguments(args, properties))
	})

	t.Run("reports typo'd and unknown keys sorted", func(t *testing.T) {
		args := map[string]any{"data_source_uid": "x", "start_rfc3339": "t", "foo": 1}
		assert.Equal(t, []string{"foo", "start_rfc3339"}, unknownArguments(args, properties))
	})

	t.Run("non-object arguments are left to the decode path", func(t *testing.T) {
		assert.Empty(t, unknownArguments("not an object", properties))
		assert.Empty(t, unknownArguments(nil, properties))
	})

}

// TestConvertToolIgnoresUnknownArguments pins the maestra-io contract: an
// argument a tool does not declare (a "reason" of any shape, a typo, a
// parameter the tool has dropped) never fails the call.
func TestConvertToolIgnoresUnknownArguments(t *testing.T) {
	_, handler, err := ConvertTool("test_tool", "A test tool", testToolHandler)
	require.NoError(t, err)
	ctx := context.Background()

	for name, extra := range map[string]any{
		"reason string":      "checking why the alert fired",
		"reason empty":       "",
		"reason null":        nil,
		"reason number":      42,
		"reason object":      map[string]any{"why": "x"},
		"reason array":       []any{"a", 1},
		"typo'd argument":    true,
		"dropped legacy arg": "ns",
	} {
		t.Run(name, func(t *testing.T) {
			key := "reason"
			switch name {
			case "typo'd argument":
				key = "optionl"
			case "dropped legacy arg":
				key = "namespace"
			}
			request := newCallToolRequest("test_tool", map[string]any{"name": "test", "value": 65, key: extra})
			result, err := handler(ctx, request)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.False(t, result.IsError, "an undeclared argument must not fail the call")
		})
	}

	t.Run("empty-params tool accepts reason", func(t *testing.T) {
		_, emptyHandler, err := ConvertTool("empty", "description", emptyToolHandler)
		require.NoError(t, err)
		result, err := emptyHandler(ctx, newCallToolRequest("empty", map[string]any{"reason": "why"}))
		require.NoError(t, err)
		assert.False(t, result.IsError)
	})

	t.Run("nil arguments succeed", func(t *testing.T) {
		_, emptyHandler, err := ConvertTool("empty", "description", emptyToolHandler)
		require.NoError(t, err)
		result, err := emptyHandler(ctx, newCallToolRequest("empty", nil))
		require.NoError(t, err)
		assert.False(t, result.IsError)
	})
}

func TestConvertToolKnownArgumentsMatchSchema(t *testing.T) {
	// The known-args check must accept exactly what the generated schema
	// declares — including fields promoted from embedded structs — so that no
	// call valid per the advertised schema starts failing.
	tool, handler, err := ConvertTool("embedded", "description", unknownArgsHandler)
	require.NoError(t, err)

	var schema map[string]any
	require.NoError(t, json.Unmarshal(inputSchemaBytes(t, tool), &schema))
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "start_rfc_3339")
	assert.Contains(t, props, "end_rfc_3339")
	assert.Contains(t, props, "data_source_uid")
	assert.NotContains(t, props, "Hidden")

	ctx := context.Background()
	args := map[string]any{"data_source_uid": "x"}
	for name := range props {
		args[name] = "y"
	}
	result, err := handler(ctx, newCallToolRequest("embedded", args))
	require.NoError(t, err)
	assert.False(t, result.IsError, "every schema-declared property must pass the unknown-args check")

	result, err = handler(ctx, newCallToolRequest("embedded", map[string]any{"data_source_uid": "x", "start_rfc3339": "now"}))
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "a near-miss key is ignored, not rejected")
}
