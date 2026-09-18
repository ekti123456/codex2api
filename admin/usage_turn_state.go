package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func parseUsageTurnStateFilter(c *gin.Context, filter *database.UsageLogFilter) bool {
	filter.TurnState = strings.TrimSpace(c.Query("turn_state"))
	switch filter.TurnState {
	case "", "received", "missing", "not_recorded":
	default:
		writeError(c, http.StatusBadRequest, "无效的 Turn-State 状态")
		return false
	}
	if raw := strings.TrimSpace(c.Query("turn_state_length")); raw != "" {
		length, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || length < 0 {
			writeError(c, http.StatusBadRequest, "Turn-State 字符数必须为非负整数")
			return false
		}
		value := int(length)
		filter.TurnStateLength = &value
	}
	return true
}
