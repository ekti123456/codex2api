package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestAccountOverloadFilterPaginationAndSelectors(test *testing.T) {
	handler, codexIDs, grokIDs := newPagedAccountsHandler(test)
	for _, accountID := range []int64{codexIDs[0], codexIDs[2], grokIDs[0]} {
		require.NoError(test, handler.db.InsertUsageLog(test.Context(), &database.UsageLogInput{
			AccountID: accountID, StatusCode: 500, ErrorMessage: "server_is_overloaded · busy", Endpoint: "/v1/responses",
		}))
	}
	require.NoError(test, handler.db.InsertUsageLog(test.Context(), &database.UsageLogInput{
		AccountID: codexIDs[1], StatusCode: 500, ErrorMessage: "other_error · server_is_overloaded", Endpoint: "/v1/responses",
	}))
	handler.db.FlushUsageLogs()
	for _, scenario := range []struct {
		filter string
		page   int
		total  int
		wantID int64
	}{
		{"marked", 1, 2, codexIDs[0]},
		{"marked", 2, 2, codexIDs[2]},
		{"unmarked", 1, 1, codexIDs[1]},
		{"all", 1, 3, codexIDs[0]},
	} {
		test.Run(fmt.Sprintf("%s/page%d", scenario.filter, scenario.page), func(test *testing.T) {
			response := invokeListAccounts(test, handler, fmt.Sprintf("/api/admin/accounts?view=page&channel=codex&page=%d&page_size=1&overload_500=%s", scenario.page, scenario.filter))
			require.Equal(test, http.StatusOK, response.Code, response.Body.String())
			var page accountsPageResponse
			require.NoError(test, json.Unmarshal(response.Body.Bytes(), &page))
			require.Equal(test, scenario.total, page.Total)
			require.Len(test, page.Accounts, 1)
			require.Equal(test, scenario.wantID, page.Accounts[0].ID)
		})
	}
	for _, scenario := range []struct {
		filter string
		search string
		want   []int64
	}{
		{"marked", "", []int64{codexIDs[0], codexIDs[2]}},
		{"unmarked", "", []int64{codexIDs[1]}},
		{"marked", "codex3", []int64{codexIDs[2]}},
		{"marked", "codex2", []int64{}},
	} {
		selected, err := handler.resolveAccountOperationSelector(test.Context(), &accountOperationSelector{
			Channel: database.UpstreamChannelCodex, Overload500: scenario.filter, Search: scenario.search,
		})
		require.NoError(test, err)
		require.ElementsMatch(test, scenario.want, selected)
	}
	response := invokeListAccounts(test, handler, "/api/admin/accounts?view=page&channel=codex&overload_500=invalid")
	require.Equal(test, http.StatusBadRequest, response.Code)
	_, err := handler.resolveAccountOperationSelector(test.Context(), &accountOperationSelector{Channel: database.UpstreamChannelCodex, Overload500: "invalid"})
	require.ErrorContains(test, err, "unsupported overload_500")
}

func TestAccountOverloadFilterCombinesWithStatus(test *testing.T) {
	query := accountPageQuery{Status: "disabled", Overload500: "marked", overloadedAccountIDs: map[int64]struct{}{1: {}, 2: {}}}
	for _, scenario := range []struct {
		item accountListSnapshotItem
		want bool
	}{
		{accountListSnapshotItem{ID: 1, Enabled: false}, true},
		{accountListSnapshotItem{ID: 2, Enabled: true}, false},
		{accountListSnapshotItem{ID: 3, Enabled: false}, false},
	} {
		require.Equal(test, scenario.want, accountListItemMatches(&scenario.item, query, database.UpstreamChannelCodex))
	}
}

func TestAccountOverloadFilterReadFailureDoesNotBecomeUnmarked(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	requestContext, cancel := context.WithCancel(test.Context())
	cancel()
	for _, filter := range []string{"marked", "unmarked"} {
		query := accountPageQuery{Overload500: filter}
		err := handler.loadAccountOverloadFilter(requestContext, &query)
		require.ErrorIs(test, err, context.Canceled)
	}
}
