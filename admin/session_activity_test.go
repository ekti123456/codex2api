package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSessionActivityAdminCurrentPageAndValidation(t *testing.T) {
	handler := &Handler{db: newTestAdminDB(t)}
	key, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, k := range []string{key, other} {
		require.True(t, handler.db.EnqueueSessionError(database.SessionErrorEvent{Identity: database.SessionErrorIdentity{Key: k, UserID: "17"}, CreatedAt: time.Now().Add(-time.Minute)}))
	}
	require.Eventually(t, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	lease := handler.db.BeginSessionActivity(key, false, time.Now())
	defer lease.Finish(false, time.Now())
	call := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/session-errors/activity", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.GetSessionActivities(ctx)
		return response
	}
	response := call(`{"keys":["` + key + `"]}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	var page database.SessionActivityPage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "running", page.Items[key].State)
	require.NotContains(t, page.Items, other)
	tooMany, _ := json.Marshal(map[string]any{"keys": make([]string, 101)})
	for _, body := range []string{`{}`, `{"keys":[]}`, `{"keys":["bad"]}`, `{"keys":["` + key + `","` + key + `"]}`, string(tooMany), strings.Repeat("x", 9000)} {
		require.Equal(t, 400, call(body).Code, body[:min(len(body), 100)])
	}
	router := gin.New()
	handler.RegisterRoutes(router)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/admin/session-errors/activity", strings.NewReader(`{"keys":["`+key+`"]}`)))
	require.Contains(t, []int{401, 503}, response.Code)
}
