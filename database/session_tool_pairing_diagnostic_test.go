package database

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestSessionToolPairingNormalizationBoundsAndCopies(t *testing.T) {
	input := &SessionToolPairingDiagnostic{Scope: "input_top_level", MissingCallCount: 12}
	for i := 0; i < 12; i++ {
		input.MissingCalls = append(input.MissingCalls, SessionMissingToolCall{
			Index: i, Path: "input[0].call_id", ItemType: "function_call_output", ExpectedCallType: "function_call",
			CallID: strings.Repeat("中文", 100), CallIDState: "present",
		})
	}
	event := normalizeServiceError(ServiceErrorEvent{AccountFailover: &SessionAccountFailoverDiagnostic{ContextCleanup: &SessionContextCleanup{ToolPairing: input}}})
	got := event.AccountFailover.ContextCleanup.ToolPairing
	require.Len(t, got.MissingCalls, 8)
	require.Equal(t, 12, got.MissingCallCount)
	require.Equal(t, 4, got.OmittedItems)
	require.True(t, got.MissingCalls[0].ValueTruncated)
	require.LessOrEqual(t, len(got.MissingCalls[0].CallID), 128)
	require.True(t, utf8.ValidString(got.MissingCalls[0].CallID))
	require.Len(t, input.MissingCalls, 12)
	require.False(t, input.MissingCalls[0].ValueTruncated)
	input.MissingCalls[0].CallID = "mutated"
	require.NotEqual(t, "mutated", got.MissingCalls[0].CallID)
	repeated := NormalizeSessionToolPairingDiagnostic(got)
	require.Equal(t, got, repeated)
	plain := &SessionToolPairingDiagnostic{MissingCalls: []SessionMissingToolCall{{CallID: " call_exact "}}}
	require.Equal(t, " call_exact ", NormalizeSessionToolPairingDiagnostic(plain).MissingCalls[0].CallID)
}
