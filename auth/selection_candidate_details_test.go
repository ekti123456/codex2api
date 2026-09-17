package auth

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSelectionCandidateDetailsBoundedAndOptional(t *testing.T) {
	trace := &SelectionTrace{}
	trace.RejectAccount(1, "account_paused")
	require.Empty(t, trace.CandidateDetails().Samples)
	trace.EnableCandidateDetails()
	for i := int64(1); i <= 100; i++ {
		trace.RejectAccount(i, "account_paused")
	}
	details := trace.CandidateDetails()
	require.Len(t, details.Samples, 20)
	require.Equal(t, 100, details.RejectionCounts["account_paused"])
	require.Equal(t, 80, details.OmittedObservations)
	details.RejectionCounts["account_paused"] = 0
	require.Equal(t, 100, trace.CandidateDetails().RejectionCounts["account_paused"])
	resume := trace.Pause()
	trace.RejectAccount(101, "account_cooldown")
	resume()
	trace.Freeze()
	trace.RejectAccount(102, "account_cooldown")
	require.Zero(t, trace.CandidateDetails().RejectionCounts["account_cooldown"])
	trace.Reset()
	require.Empty(t, trace.CandidateDetails().Samples)
}
