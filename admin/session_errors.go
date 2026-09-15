package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func (handler *Handler) GetSessionErrors(ctx *gin.Context) {
	query := database.SessionErrorQuery{UserID: strings.TrimSpace(ctx.Query("user_id")), SessionID: strings.TrimSpace(ctx.Query("session_id")), Cursor: ctx.Query("cursor"), LockedOnly: ctx.Query("locked") == "true", LockState: ctx.DefaultQuery("lock_state", "unlocked"), Limit: 20}
	query.Model = strings.TrimSpace(ctx.Query("model"))
	query.Account = strings.TrimSpace(ctx.Query("account"))
	if len(query.Model) > 256 || len(query.Account) > 256 {
		writeError(ctx, http.StatusBadRequest, "模型或账号查询条件过长")
		return
	}
	if len(query.UserID) > 255 || len(query.SessionID) > 256 || !database.ValidateServiceErrorCursor(query.Cursor) || ctx.Query("locked") != "" && ctx.Query("locked") != "true" && ctx.Query("locked") != "false" || query.LockState != "unlocked" && query.LockState != "locked" && query.LockState != "all" {
		writeError(ctx, http.StatusBadRequest, "查询条件无效")
		return
	}
	if raw := ctx.Query("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeError(ctx, http.StatusBadRequest, "每页数量必须在 1 到 100 之间")
			return
		}
		query.Limit = limit
	}
	if handler.db == nil {
		writeError(ctx, http.StatusServiceUnavailable, "会话统计暂不可用")
		return
	}
	lookup, cancel := context.WithTimeout(ctx.Request.Context(), 3*time.Second)
	defer cancel()
	page, err := handler.db.ListSessionErrors(lookup, query)
	if err != nil {
		writeError(ctx, http.StatusServiceUnavailable, "会话统计查询失败，请稍后重试")
		return
	}
	ctx.JSON(http.StatusOK, page)
}

func (handler *Handler) GetSessionActivities(ctx *gin.Context) {
	var request struct {
		Keys []string `json:"keys"`
	}
	ctx.Request.Body = http.MaxBytesReader(ctx.Writer, ctx.Request.Body, 8192)
	if ctx.ShouldBindJSON(&request) != nil || len(request.Keys) == 0 || len(request.Keys) > 100 {
		writeError(ctx, http.StatusBadRequest, "请选择当前页的 1 到 100 个会话")
		return
	}
	seen := make(map[string]bool, len(request.Keys))
	for _, key := range request.Keys {
		if !database.ValidSessionOperationKey(key) || seen[key] {
			writeError(ctx, http.StatusBadRequest, "会话标识无效或重复")
			return
		}
		seen[key] = true
	}
	if handler.db == nil {
		writeError(ctx, http.StatusServiceUnavailable, "会话状态暂不可用")
		return
	}
	lookup, cancel := context.WithTimeout(ctx.Request.Context(), 2*time.Second)
	defer cancel()
	page, err := handler.db.SessionActivities(lookup, request.Keys, time.Now())
	if err != nil {
		writeError(ctx, http.StatusServiceUnavailable, "会话状态查询失败，请稍后重试")
		return
	}
	ctx.Header("Cache-Control", "no-store")
	ctx.JSON(http.StatusOK, page)
}

func (handler *Handler) SetSessionBlacklist(ctx *gin.Context) {
	var request struct {
		Keys   []string `json:"keys"`
		Locked *bool    `json:"locked"`
	}
	ctx.Request.Body = http.MaxBytesReader(ctx.Writer, ctx.Request.Body, 16384)
	if ctx.ShouldBindJSON(&request) != nil || request.Locked == nil || len(request.Keys) == 0 || len(request.Keys) > 100 {
		writeError(ctx, http.StatusBadRequest, "请选择 1 到 100 个会话并明确锁定或解锁操作")
		return
	}
	seen := make(map[string]bool)
	for _, key := range request.Keys {
		if !database.ValidSessionOperationKey(key) || seen[key] {
			writeError(ctx, http.StatusBadRequest, "会话标识无效或重复")
			return
		}
		seen[key] = true
	}
	if handler.db == nil {
		writeError(ctx, http.StatusServiceUnavailable, "会话黑名单暂不可用")
		return
	}
	operation, cancel := context.WithTimeout(ctx.Request.Context(), 3*time.Second)
	defer cancel()
	if err := handler.db.SetSessionBlacklist(operation, request.Keys, *request.Locked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(ctx, http.StatusNotFound, "部分会话不存在，请刷新后重试；本次未修改任何会话")
			return
		}
		writeError(ctx, http.StatusServiceUnavailable, "会话黑名单更新失败，请刷新确认后重试")
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"updated": len(request.Keys), "locked": *request.Locked})
}
