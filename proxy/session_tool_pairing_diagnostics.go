package proxy

import (
	"strconv"
	"strings"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

// Diagnose pairing without reconstruction: calls may occur later in input;
// standalone function messages and hosted search outputs need no matching call.
// Nested user data is not protocol.
func inspectPreservedToolPairing(input gjson.Result) (*database.SessionToolPairingDiagnostic, bool) {
	items := input.Array()
	report := &database.SessionToolPairingDiagnostic{Scope: "input_top_level", InputItems: len(items)}
	calls, searches := make(map[string]bool), make(map[string]bool)
	for _, item := range items {
		typ, id := item.Get("type").String(), item.Get("call_id").String()
		if strings.HasSuffix(typ, "_call") {
			report.CallItems++
			calls[id] = true
		}
		if typ == "tool_search_call" {
			searches[id] = true
		}
	}
	missingOutput := false
	for index, item := range items {
		typ, id := item.Get("type").String(), item.Get("call_id")
		expected := ""
		if strings.HasSuffix(typ, "_call_output") {
			report.OutputItems++
			if !responseContextStandaloneFunctionOutput(item) && !calls[id.String()] {
				missingOutput = true
				expected = strings.TrimSuffix(typ, "_output")
			}
		} else if typ == "tool_search_output" {
			report.OutputItems++
			if item.Get("execution").String() == "client" && id.String() != "" && !searches[id.String()] {
				expected = "tool_search_call"
			}
		}
		if expected == "" {
			continue
		}
		report.MissingCallCount++
		if len(report.MissingCalls) == database.MaxSessionToolPairingDetails {
			report.OmittedItems++
			continue
		}
		state := "present"
		switch {
		case !id.Exists():
			state = "missing"
		case id.Type == gjson.Null:
			state = "null"
		case id.Type != gjson.String:
			state = "non_string"
		case id.String() == "":
			state = "empty"
		}
		report.MissingCalls = append(report.MissingCalls, database.SessionMissingToolCall{
			Index: index, Path: "input[" + strconv.Itoa(index) + "].call_id", ItemType: typ,
			ExpectedCallType: expected, CallID: id.String(), CallIDState: state,
		})
	}
	if report.MissingCallCount == 0 {
		return nil, false
	}
	return database.NormalizeSessionToolPairingDiagnostic(report), missingOutput
}

func preservedToolPairingBlockers(report *database.SessionContextCleanup) []database.SessionContextBlocker {
	if report == nil || report.ToolPairing == nil || report.ToolPairing.MissingCallCount == 0 {
		return nil
	}
	var blockers []database.SessionContextBlocker
	for _, missing := range report.ToolPairing.MissingCalls {
		blockers = append(blockers, database.SessionContextBlocker{Kind: "missing_tool_call", Path: missing.Path, ItemType: missing.ItemType})
	}
	return blockers
}
