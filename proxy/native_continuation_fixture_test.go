package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

// Native continuations use the same persistent response mapping as production.
func nativeContinuationHandler(t *testing.T, store *auth.Store) *Handler {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "continuation.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
}

func nativeContinuationBody(t *testing.T, root string, body []byte) []byte {
	t.Helper()
	metadata := map[string]any{
		"session_id": root, "thread_id": root,
		"x-codex-turn-metadata": map[string]any{
			"session_id": root, "thread_id": root, "thread_source": "user", "request_kind": "turn",
			"window_id": root + ":0", "window_number": 0,
		},
	}
	for key, value := range metadata {
		var err error
		body, err = sjson.SetBytes(body, "client_metadata."+key, value)
		require.NoError(t, err)
	}
	return body
}

// Seed a previously completed response with authenticated account/root ownership,
// rather than allowing an arbitrary client-supplied upstream response ID.
func nativeContinuationAlias(t *testing.T, h *Handler, account *auth.Account, root, original string) string {
	t.Helper()
	body := nativeContinuationBody(t, root, []byte(`{"model":"gpt-5.5","input":"earlier context"}`))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header = continuationFixtureHeaders(h)
	identity := h.resolveRequestSessionIdentityForContext(c, body)
	key := sessionAffinityKey(identity.affinityID, requestAPIKeyID(c))
	require.Equal(t, root, identity.affinityID)
	h.store.BindSessionAffinity(key, account, "")
	_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{
		AccountID: account.ID(), ThreadID: root, NumberKnown: true, LastSeen: time.Now(),
	})
	require.NoError(t, err)
	state := responseIdentityFrom(c.Request.Context())
	require.NotNil(t, state)
	alias, err := state.issue(c.Request.Context(), account, original)
	require.NoError(t, err)
	require.True(t, h.db.IsManagedCodexResponseID(alias))
	setResponseCache(state.owner, alias, []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"earlier context"}`)})
	return alias
}
