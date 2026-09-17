package admin

import (
	"context"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"net/http"
	"time"
)

func (h *Handler) GetSessionAutoLockSettings(c *gin.Context) {
	if h.db == nil {
		writeError(c, 503, "会话自动锁定设置暂不可用")
		return
	}
	c.JSON(http.StatusOK, h.db.GetSessionAutoLockSettings())
}

func (h *Handler) SetSessionAutoLockSettings(c *gin.Context) {
	var input struct {
		Enabled   *bool `json:"enabled"`
		Threshold *int  `json:"threshold"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	if c.ShouldBindJSON(&input) != nil || input.Enabled == nil || input.Threshold == nil || *input.Threshold < 1 || *input.Threshold > 10000 {
		writeError(c, 400, "请指定开关状态，连续错误次数必须为 1 到 10000 的整数")
		return
	}
	if h.db == nil {
		writeError(c, 503, "会话自动锁定设置暂不可用")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := h.db.SetSessionAutoLockSettings(ctx, database.SessionAutoLockSettings{Enabled: *input.Enabled, Threshold: *input.Threshold}); err != nil {
		writeError(c, 503, "保存会话自动锁定设置失败")
		return
	}
	c.JSON(200, h.db.GetSessionAutoLockSettings())
}
