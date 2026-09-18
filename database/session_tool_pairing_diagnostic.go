package database

import "strings"

const MaxSessionToolPairingDetails = 8

// Only protocol identifiers and counts, never tool names, arguments or outputs.
type SessionToolPairingDiagnostic struct {
	Scope            string                   `json:"scope"`
	InputItems       int                      `json:"input_items"`
	CallItems        int                      `json:"call_items"`
	OutputItems      int                      `json:"output_items"`
	MissingCallCount int                      `json:"missing_call_count"`
	MissingCalls     []SessionMissingToolCall `json:"missing_calls"`
	OmittedItems     int                      `json:"omitted_items,omitempty"`
}

type SessionMissingToolCall struct {
	Index            int    `json:"index"`
	Path             string `json:"path"`
	ItemType         string `json:"item_type"`
	ExpectedCallType string `json:"expected_call_type"`
	CallID           string `json:"call_id"`
	CallIDState      string `json:"call_id_state"`
	ValueTruncated   bool   `json:"value_truncated,omitempty"`
}

// Copy and bound before snapshots are shared with asynchronous log writers.
// Preserve normal call IDs verbatim, including whitespace, for exact comparison.
func NormalizeSessionToolPairingDiagnostic(input *SessionToolPairingDiagnostic) *SessionToolPairingDiagnostic {
	if input == nil {
		return nil
	}
	result := *input
	result.Scope = serviceErrorString(result.Scope, 64)
	count := min(len(input.MissingCalls), MaxSessionToolPairingDetails)
	result.MissingCalls = append([]SessionMissingToolCall(nil), input.MissingCalls[:count]...)
	result.OmittedItems += len(input.MissingCalls) - count
	for i := range result.MissingCalls {
		item := &result.MissingCalls[i]
		bound := func(value string, limit int) string {
			if len(value) > limit {
				item.ValueTruncated = true
				return strings.ToValidUTF8(value[:limit], "")
			}
			return value
		}
		item.Path = bound(item.Path, 64)
		item.ItemType = bound(item.ItemType, 64)
		item.ExpectedCallType = bound(item.ExpectedCallType, 64)
		item.CallID = bound(item.CallID, 128)
		item.CallIDState = bound(item.CallIDState, 32)
	}
	return &result
}
