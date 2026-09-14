package proxy

import "math"

// Called inside the user's admission transaction, counting confirmed windows
// and pending reservations. Existing grants retain their frozen multiplier.
func (input windowControlRequest) expansionMultiplier(ordinal int) float64 {
	if input.MultiplierStep == 0 {
		return input.Multiplier // Compatibility with older signed NewAPI senders.
	}
	return math.Min(input.Multiplier, math.Round((1+float64(ordinal)*input.MultiplierStep)*1e6)/1e6)
}
