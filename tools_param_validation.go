package mcpgrafana

import (
	"sort"
	"strings"
)

// unknownArguments returns the top-level argument keys that do not match any
// schema property. ConvertTool records them for diagnostics and otherwise
// ignores them: an undeclared argument never fails a call. Matching is case-insensitive, mirroring
// encoding/json's fallback field matching. Non-object argument shapes are left
// to the decode path to report.
func unknownArguments(args any, properties map[string]any) []string {
	argMap, ok := args.(map[string]any)
	if !ok {
		return nil
	}
	var unknown []string
	for key := range argMap {
		if _, ok := properties[key]; ok {
			continue
		}
		if !hasKeyFold(properties, key) {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// hasKeyFold reports whether m contains a key equal to k under Unicode case
// folding.
func hasKeyFold(m map[string]any, k string) bool {
	for name := range m {
		if strings.EqualFold(name, k) {
			return true
		}
	}
	return false
}
