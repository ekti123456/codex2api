package admin

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageTurnStateFilterValidation(t *testing.T) {
	for _, tc := range []struct {
		query string
		valid bool
	}{
		{"", true}, {"turn_state=received&turn_state_length=292", true},
		{"turn_state_length=217", true}, {"turn_state=missing&turn_state_length=0", true},
		{"turn_state=not_recorded", true}, {"turn_state=invalid", false},
		{"turn_state_length=-1", false}, {"turn_state_length=1.5", false},
		{"turn_state_length=2147483648", false}, {"turn_state_length=abc", false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/usage/logs?"+tc.query, nil)
			filter, valid := parseUsageLogsFilter(c, time.Now().Add(-time.Hour), time.Now())
			require.Equal(t, tc.valid, valid)
			if !valid {
				require.Equal(t, 400, w.Code)
			}
			if tc.query == "turn_state=received&turn_state_length=292" {
				require.Equal(t, "received", filter.TurnState)
				require.Equal(t, 292, *filter.TurnStateLength)
			}
		})
	}
}
