package proxy

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageTurnStartIngressAndRetry(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "turns.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := &Handler{db: db}
	started := time.Now().Add(time.Millisecond).UnixMilli()
	newRequest := func(turn string, input string) (*gin.Context, []byte, *usageRequestDiagnostics) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Request.Header.Set("Authorization", "Bearer test-owner")
		body := []byte(fmt.Sprintf(`{"input":%s,"client_metadata":{"thread_id":%q,"x-codex-turn-metadata":{"thread_id":%q,"turn_id":%q,"turn_started_at_unix_ms":%d}}}`, input, testRootSessionA, testRootSessionA, turn, started))
		captureUsageRequestIngress(c, body)
		s := usageRequestDiagnosticState(c)
		s.Resolved = &usageRequestResolution{RootState: "resolved", ThreadSource: "user", RequestKind: "turn"}
		return c, body, s
	}
	c, body, state := newRequest(testLeafSessionA, `"为项目加入可视化UI,是需要每个工程都加吗"`)
	h.captureUsageTurnStart(c, body, state)
	input := &database.UsageLogInput{StatusCode: 500}
	populateUsageRequestDiagnostics(c, input)
	require.NotNil(t, input.IsTurnFirstRequest)
	require.True(t, *input.IsTurnFirstRequest)
	require.Equal(t, "为项目加入可视化UI,是需要每个工程都加…", input.TurnPromptPreview)
	require.Equal(t, testLeafSessionA, input.TurnID)
	// Same logical request, new upstream attempt/account: no second badge.
	beginUsageSelectionAttempt(c, 2)
	retry := &database.UsageLogInput{StatusCode: 200, AccountID: 99}
	populateUsageRequestDiagnostics(c, retry)
	require.NotNil(t, retry.IsTurnFirstRequest)
	require.False(t, *retry.IsTurnFirstRequest)
	require.Empty(t, retry.TurnPromptPreview)
	// Same turn after tool output; the old user input is not a new question.
	c2, body2, state2 := newRequest(testLeafSessionA, `[{"role":"user","content":"旧问题"},{"type":"function_call_output","output":"结果"}]`)
	h.captureUsageTurnStart(c2, body2, state2)
	require.NotNil(t, state2.TurnStart.First)
	require.False(t, *state2.TurnStart.First)
	// A new user turn is first even on the same thread/connection.
	resetPromptPolicyRequestCorrelationID(c)
	resetCodexInternalRequestClassificationFrame(c)
	c.Request.Header.Set(codexTurnMetadataHeader, fmt.Sprintf(`{"thread_id":%q,"turn_id":%q,"turn_started_at_unix_ms":%d}`, testRootSessionA, testLeafSessionB, started))
	newBody := []byte(`{"input":"下一轮"}`)
	captureUsageRequestIngress(c, newBody)
	fresh := usageRequestDiagnosticState(c)
	fresh.Resolved = &usageRequestResolution{RootState: "resolved", ThreadSource: "user", RequestKind: "turn"}
	h.captureUsageTurnStart(c, newBody, fresh)
	require.NotSame(t, state, fresh)
	require.NotNil(t, fresh.TurnStart.First)
	require.True(t, *fresh.TurnStart.First)
	require.Equal(t, "下一轮", fresh.TurnStart.Preview)
}

func TestUsageTurnStartUnknownAndInternal(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "unknown.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := &Handler{db: db}
	for _, name := range []string{"compaction", "tool-only", "passive", "subagent", "conflict", "missing-turn", "pre-upgrade", "missing-time"} {
		t.Run(name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			c.Request.Header.Set("Authorization", "Bearer "+name)
			started := time.Now().UnixMilli()
			if name == "pre-upgrade" {
				started -= 3600000
			}
			metadata := fmt.Sprintf(`{"thread_id":%q,"turn_id":%q,"turn_started_at_unix_ms":%d}`, testRootSessionA, testLeafSessionA, started)
			c.Request.Header.Set(codexTurnMetadataHeader, metadata)
			body := []byte(`{"input":"请检查"}`)
			if name == "tool-only" {
				body = []byte(`{"input":[{"type":"function_call_output","output":"结果"}]}`)
			}
			if name == "compaction" {
				body = []byte(`{"input":"You are performing a CONTEXT CHECKPOINT COMPACTION."}`)
			}
			if name == "conflict" {
				body = []byte(fmt.Sprintf(`{"input":"请检查","client_metadata":{"turn_id":%q}}`, testLeafSessionB))
			}
			if name == "missing-turn" {
				c.Request.Header.Set(codexTurnMetadataHeader, `{}`)
			}
			if name == "missing-time" {
				c.Request.Header.Set(codexTurnMetadataHeader, fmt.Sprintf(`{"thread_id":%q,"turn_id":%q}`, testRootSessionA, testLeafSessionA))
			}
			captureUsageRequestIngress(c, body)
			s := usageRequestDiagnosticState(c)
			s.Resolved = &usageRequestResolution{RootState: "resolved", ThreadSource: "user", RequestKind: "turn"}
			if name == "passive" {
				s.Resolved.Passive = true
			}
			if name == "subagent" {
				s.Resolved.SubagentKind = "review"
			}
			if name == "compaction" {
				s.ResponsesInput.Mode = "metadata_only"
			}
			h.captureUsageTurnStart(c, body, s)
			input := &database.UsageLogInput{}
			populateUsageRequestDiagnostics(c, input)
			require.Nil(t, input.IsTurnFirstRequest)
			require.Empty(t, input.TurnPromptPreview)
		})
	}
}
