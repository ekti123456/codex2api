package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountHealthBarsExposeOverloadAndMatchingBounds(test *testing.T) {
	db := newTestAdminDB(test)
	err := db.InsertUsageLog(context.Background(), &database.UsageLogInput{
		AccountID: 1, StatusCode: 500, ErrorMessage: "server_is_overloaded · busy", Endpoint: "/v1/responses",
	})
	require.NoError(test, err)
	db.FlushUsageLogs()
	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	request, _ := gin.CreateTestContext(recorder)
	request.Request = httptest.NewRequest(http.MethodGet, "/api/accounts/health-bars?ids=1", nil)
	before := time.Now()
	handler.GetAccountHealthBars(request)
	after := time.Now()
	require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		Buckets      map[string][]database.AccountHealthBucket `json:"buckets"`
		BlockCount   int                                       `json:"block_count"`
		BlockMinutes int                                       `json:"block_minutes"`
	}
	require.NoError(test, json.Unmarshal(recorder.Body.Bytes(), &response))
	buckets := response.Buckets["1"]
	require.Len(test, buckets, 20)
	require.Equal(test, 1, buckets[19].Overloaded500)
	require.Equal(test, 1, buckets[19].Failed)
	require.Equal(test, 20, response.BlockCount)
	require.Equal(test, 10, response.BlockMinutes)
	require.Equal(test, 200*time.Minute, buckets[19].EndAt.Sub(buckets[0].StartAt))
	require.False(test, buckets[19].EndAt.Before(before))
	require.False(test, buckets[19].EndAt.After(after))
	for index, bucket := range buckets {
		require.Equal(test, 10*time.Minute, bucket.EndAt.Sub(bucket.StartAt))
		if index > 0 {
			require.Equal(test, buckets[index-1].EndAt, bucket.StartAt)
		}
	}
}
