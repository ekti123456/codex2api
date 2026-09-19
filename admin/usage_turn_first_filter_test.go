package admin

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageTurnFirstFilterValidation(t *testing.T) {
	for _, scenario := range []struct {
		value string
		valid bool
	}{{"", true}, {"true", true}, {"false", true}, {"unknown", true}, {"1", false}, {"first", false}} {
		t.Run("turn="+scenario.value, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/usage/logs?retry=false&turn_first="+scenario.value, nil)
			filter, valid := parseUsageLogsFilter(c, time.Now().Add(-time.Hour), time.Now())
			require.Equal(t, scenario.valid, valid)
			if !valid {
				require.Equal(t, 400, w.Code)
				return
			}
			require.Equal(t, scenario.value, filter.TurnFirst)
			require.NotNil(t, filter.RetryOnly)
			require.False(t, *filter.RetryOnly)
		})
	}
}
