package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWindowTieredPricingReservationsCapAndFixedTariffs(t *testing.T) {
	handler := newWindowAuthorizationHandler(t)
	makeRequest := func(root string, step float64) (*gin.Context, *httptest.ResponseRecorder) {
		body, err := json.Marshal(windowControlRequest{Operation: "quote_tiered", AllowExpansion: true, ExtraLimit: 6, Multiplier: 1.5, MultiplierStep: step, ReservationID: root})
		require.NoError(t, err)
		meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: promptSessionTestFingerprint(root), ThreadSource: "user", RequestKind: "turn"}
		return windowExpansionTestContext(t, "/v1/session-windows", body, meta)
	}
	readGrant := func(response *httptest.ResponseRecorder) signedWindowGrant {
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var result struct {
			Ticket string `json:"ticket"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		grant, err := decodeWindowGrant("integration-secret", result.Ticket)
		require.NoError(t, err)
		return grant
	}
	ordinary, response := makeRequest("ordinary", 0.1)
	handler.ControlNewAPIUserWindows(ordinary)
	require.Equal(t, 1.0, readGrant(response).Grant.Multiplier)
	var requests [6]*gin.Context
	var responses [6]*httptest.ResponseRecorder
	for i := range requests {
		requests[i], responses[i] = makeRequest(fmt.Sprint("extra-", i), 0.1)
	}
	var workers sync.WaitGroup
	for i := range requests {
		workers.Go(func() { handler.ControlNewAPIUserWindows(requests[i]) })
	}
	workers.Wait()
	prices := make([]float64, 0, len(responses))
	for _, response := range responses {
		prices = append(prices, readGrant(response).Grant.Multiplier)
	}
	sort.Float64s(prices)
	require.Equal(t, []float64{1.1, 1.2, 1.3, 1.4, 1.5, 1.5}, prices)
	old := readGrant(responses[0])
	retry, response := makeRequest("extra-0", 0.2)
	handler.ControlNewAPIUserWindows(retry)
	require.Equal(t, old.Grant, readGrant(response).Grant, "retry and policy changes must retain the authorized tariff")
	seventh, response := makeRequest("extra-6", 0.1)
	handler.ControlNewAPIUserWindows(seventh)
	require.Equal(t, http.StatusBadRequest, response.Code, "a price cap must not remove the window count limit")
	require.NoError(t, handler.db.UpdateUserWindowAdmissions(t.Context(), cache.PromptSessionLimitSubject("test-platform", "42"), func(state *database.UserWindowAdmissionState) error {
		for _, grant := range state.Windows {
			if grant.Expanded {
				grant.PendingUntil = time.Now().Add(-time.Minute)
			}
		}
		return nil
	}))
	fresh, response := makeRequest("after-expired-reservations", 0.2)
	handler.ControlNewAPIUserWindows(fresh)
	require.Equal(t, 1.2, readGrant(response).Grant.Multiplier)
}

func TestWindowTieredPricingCustomSteps(t *testing.T) {
	for _, item := range []struct {
		step, cap float64
		ordinal   int
		want      float64
	}{
		{0.2, 1.5, 1, 1.2}, {0.2, 1.5, 2, 1.4}, {0.2, 1.5, 3, 1.5},
		{0.000001, 1.5, 1, 1.000001}, {0.1, 1.25, 3, 1.25}, {9, 1.5, 1, 1.5}, {0, 1.5, 1, 1.5},
	} {
		require.Equal(t, item.want, (windowControlRequest{MultiplierStep: item.step, Multiplier: item.cap}).expansionMultiplier(item.ordinal))
	}
}
