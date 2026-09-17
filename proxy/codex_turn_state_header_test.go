package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestTurnStateMetadataHTTPClientKeepsFirstToken(t *testing.T) {
	for _, mode := range []struct {
		name             string
		stream, buffered bool
	}{
		{"stream", true, false},
		{"buffered_retry", true, true},
		{"json", false, false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			h, account, _, _ := failoverTestSetup(t, true)
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			settings := CurrentRuntimeSettings()
			settings.CodexForceWebsocket = false
			settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: mode.buffered, CatchAll: mode.buffered}
			ApplyRuntimeSettings(settings)
			oldResin := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(oldResin) })
			type incoming struct{ header, metadata string }
			seen := make(chan incoming, 4)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				seen <- incoming{r.Header.Get(codexTurnStateHeader), gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String()}
				index := calls
				calls++
				if index == 2 {
					index = 1 // Same real value must reuse the persisted alias.
				}
				w.Header().Set("Content-Type", "text/event-stream")
				// No HTTP turn-state header: it exists only inside upstream SSE.
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\"}\n\n"+
					"data: {\"type\":\"codex.response.metadata\",\"headers\":{\"X-Codex-Turn-State\":\"real-metadata-%d\"}}\n\n"+
					"data: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":\"later-metadata-%d\"}}\n\n"+
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"+
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-header\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", index, index)
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "turn-state-header"})
			root, turn := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
			_, body := failoverTestRequest(t, h)
			body = bytes.ReplaceAll(body, []byte(continuityTestThread), []byte(root))
			body, _ = sjson.SetBytes(body, "stream", mode.stream)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", turn)
			probe, _ := newTurnStateTestContext(t)
			probe.Set(contextAPIKeyID, int64(101))
			probe.Request.Header.Set("Authorization", "Bearer test-user-key")
			identity := h.resolveRequestSessionIdentityForContext(probe, body)
			key := capacityAwareSessionAffinityKey(identity, 101)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: account.ID(), ThreadID: root, NumberKnown: true, LastSeen: time.Now()})
			require.NoError(t, err)
			h.store.BindSessionAffinity(key, account, "")
			var clientSaved string
			var returned []string
			for step := 0; step < 4; step++ {
				if step == 3 {
					// A new turn forgets the previous turn's token (Codex OnceLock).
					clientSaved = ""
					body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", uuid.Must(uuid.NewV7()).String())
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Set(contextAPIKeyID, int64(101))
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("Authorization", "Bearer test-user-key")
				c.Request.Header.Set(codexTurnMetadataHeader, gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").Raw)
				if clientSaved != "" {
					c.Request.Header.Set(codexTurnStateHeader, clientSaved)
				}
				h.Responses(c)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				alias := recorder.Result().Header.Get(codexTurnStateHeader)
				require.True(t, h.db.IsManagedCodexTurnStateAlias(alias), "actual response header missing; step=%d", step)
				record, found, err := h.db.ReadCodexTurnState(t.Context(), alias)
				require.NoError(t, err)
				require.True(t, found)
				index := step
				if index == 2 {
					index = 1
				}
				require.Equal(t, fmt.Sprintf("real-metadata-%d", index), record.Real, "keep first metadata; do not overwrite with later-metadata")
				if clientSaved == "" {
					clientSaved = alias
				}
				returned = append(returned, alias)
				got := <-seen
				if step == 1 || step == 2 {
					require.Equal(t, "real-metadata-0", got.header)
					require.Equal(t, "real-metadata-0", got.metadata)
				} else {
					require.Empty(t, got.header)
					require.Empty(t, got.metadata)
				}
				require.NotContains(t, recorder.Body.String(), "real-metadata-")
				require.NotContains(t, recorder.Body.String(), "later-metadata-")
			}
			require.NotEqual(t, returned[0], returned[1])
			require.Equal(t, returned[1], returned[2])
			require.NotEqual(t, returned[0], returned[3])
		})
	}
}

func TestTurnStateMetadataHeaderRequiresMaskedAlias(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	c, _, _ := aliasRequest(t, h, 101, "turn", "", false)
	account := &auth.Account{DBID: 71, AccountID: "a"}
	alias, err := turnStateSessionFrom(c.Request.Context()).issue(c.Request.Context(), account, "real", "response_metadata")
	require.NoError(t, err)
	for _, kind := range []string{"response.metadata", "codex.response.metadata", "responsesapi.response.metadata"} {
		for _, value := range []string{fmt.Sprintf("%q", alias), fmt.Sprintf("[%q]", alias)} {
			header := make(http.Header)
			stageTurnStateMetadataHeader(c.Request.Context(), header, gjson.Parse(fmt.Sprintf(`{"type":%q,"headers":{"X-CODEX-TURN-STATE":%s}}`, kind, value)))
			require.Equal(t, alias, header.Get(codexTurnStateHeader))
		}
	}
	for _, value := range []string{`"raw-upstream-state"`, `null`, `123`, `[]`, fmt.Sprintf(`[%q,%q]`, alias, alias)} {
		header := make(http.Header)
		stageTurnStateMetadataHeader(c.Request.Context(), header, gjson.Parse(`{"type":"response.metadata","headers":{"x-codex-turn-state":`+value+`}}`))
		require.Empty(t, header.Get(codexTurnStateHeader))
	}
	header := http.Header{codexTurnStateHeader: {"existing-header"}}
	stageTurnStateMetadataHeader(c.Request.Context(), header, gjson.Parse(fmt.Sprintf(`{"type":"response.metadata","headers":{"x-codex-turn-state":%q}}`, alias)))
	require.Equal(t, "existing-header", header.Get(codexTurnStateHeader))
}
