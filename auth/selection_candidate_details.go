package auth

// Counts are rejection observations, not distinct accounts: scheduling retries
// may evaluate one account more than once. Samples are bounded and admin-only.
type SelectionCandidateDetails struct {
	RejectionCounts     map[string]int                `json:"rejection_counts"`
	Samples             []SelectionCandidateRejection `json:"samples,omitempty"`
	OmittedObservations int                           `json:"omitted_observations,omitempty"`
}

type SelectionCandidateRejection struct {
	AccountID int64  `json:"account_id"`
	Reason    string `json:"reason"`
}

func (trace *SelectionTrace) EnableCandidateDetails() {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.candidateDetails = &SelectionCandidateDetails{RejectionCounts: make(map[string]int)}
}

func (trace *SelectionTrace) RejectAccount(accountID int64, reason string) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if !trace.rejectLocked(reason) {
		return
	}
	details := trace.candidateDetails
	if details == nil || trace.frozen || trace.suspended > 0 || accountID <= 0 {
		return
	}
	for _, sample := range details.Samples {
		if sample.AccountID == accountID && sample.Reason == reason {
			return
		}
	}
	if len(details.Samples) >= 20 {
		details.OmittedObservations++
		return
	}
	details.Samples = append(details.Samples, SelectionCandidateRejection{accountID, reason})
}

func (trace *SelectionTrace) CandidateDetails() SelectionCandidateDetails {
	result := SelectionCandidateDetails{RejectionCounts: make(map[string]int)}
	if trace == nil {
		return result
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.candidateDetails != nil {
		for reason, count := range trace.candidateDetails.RejectionCounts {
			result.RejectionCounts[reason] = count
		}
		result.Samples = append([]SelectionCandidateRejection(nil), trace.candidateDetails.Samples...)
		result.OmittedObservations = trace.candidateDetails.OmittedObservations
	}
	return result
}
