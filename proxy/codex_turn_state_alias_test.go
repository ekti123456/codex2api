package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestTurnStateAliasHTTPAndNativeWebsocketFailover(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "native_websocket"}[ws], func(t *testing.T) {
			h, owner, target, _ := failoverTestSetup(t, true)
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			oldResin := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(oldResin) })
			type seenRequest struct{ auth, header, metadata string }
			seen := make(chan seenRequest, 16)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				seen <- seenRequest{r.Header.Get("Authorization"), r.Header.Get(codexTurnStateHeader), gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String()}
				real := "real-" + r.Header.Get("Authorization")
				w.Header().Set(codexTurnStateHeader, real)
				if strings.HasSuffix(r.URL.Path, "/compact") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"compact-alias","output":[{"type":"compaction","encrypted_content":"opaque"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				payload, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]string{"x-codex-turn-state": real}})
				_, _ = io.WriteString(w, "data: "+string(payload)+"\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_alias\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "alias-test"})
			root := uuid.Must(uuid.NewV7()).String()
			turn := uuid.Must(uuid.NewV7()).String()
			_, body := failoverTestRequest(t, h)
			body = bytes.ReplaceAll(body, []byte(continuityTestThread), []byte(root))
			body, _ = sjson.SetBytes(body, "stream", true)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", turn)
			// Seed a real persistent root, so this exercises restoration rather than
			// the unrelated first-session age/admission policy.
			probe, _ := newTurnStateTestContext(t)
			probe.Set(contextAPIKeyID, int64(101))
			probe.Request.Header.Set("Authorization", "Bearer test-user-key")
			identity := h.resolveRequestSessionIdentityForContext(probe, body)
			key := capacityAwareSessionAffinityKey(identity, 101)
			require.NotEmpty(t, key)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: root, NumberKnown: true, LastSeen: time.Now()})
			require.NoError(t, err)
			h.store.BindSessionAffinity(key, owner, "")
			var conn *websocket.Conn
			if ws {
				engine := gin.New()
				engine.GET("/v1/responses", func(c *gin.Context) { c.Set(contextAPIKeyID, int64(101)); h.ResponsesWebSocket(c) })
				server := httptest.NewServer(engine)
				t.Cleanup(server.Close)
				conn, _, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-user-key"}})
				require.NoError(t, err)
				defer conn.Close()
			}
			var aliasA, aliasB string
			for step := 0; step < 5; step++ {
				if step == 2 {
					_, _, err = h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: root, LossyContextRestart: true, PreserveRestartInput: true})
					require.NoError(t, err)
					h.store.UnbindSessionAffinity(key, owner.ID())
					h.store.BindSessionAffinity(key, target, "")
				}
				sentAlias := ""
				if step > 0 {
					sentAlias = aliasA
				}
				if step == 4 {
					sentAlias = aliasB
				}
				requestBody := bytes.Clone(body)
				if sentAlias != "" {
					requestBody, _ = sjson.SetBytes(requestBody, "client_metadata.x-codex-turn-state", sentAlias)
				}
				var output, returned string
				if ws {
					requestBody, _ = sjson.SetBytes(requestBody, "type", "response.create")
					require.NoError(t, conn.WriteMessage(websocket.TextMessage, requestBody))
					require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
					for {
						_, event, readErr := conn.ReadMessage()
						require.NoError(t, readErr)
						output += string(event)
						kind := gjson.GetBytes(event, "type").String()
						require.NotEqual(t, "error", kind, "step=%d %s", step, event)
						require.NotEqual(t, "response.failed", kind, "%s", event)
						if kind == "response.metadata" {
							returned = gjson.GetBytes(event, "headers.x-codex-turn-state").String()
						}
						if kind == "response.completed" {
							break
						}
					}
				} else {
					r := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(r)
					c.Set(contextAPIKeyID, int64(101))
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
					c.Request.Header.Set("Authorization", "Bearer test-user-key")
					if sentAlias != "" {
						c.Request.Header.Set(codexTurnStateHeader, sentAlias)
					}
					if step == 4 {
						c.Request.URL.Path = "/v1/responses/compact"
						h.ResponsesCompact(c)
					} else {
						h.Responses(c)
					}
					require.Equal(t, 200, r.Code, "step=%d %s", step, r.Body.String())
					returned = r.Header().Get(codexTurnStateHeader)
					output = r.Body.String()
				}
				require.True(t, database.ValidCodexTurnStateAlias(returned), "step=%d output=%s", step, output)
				require.NotContains(t, output, "real-Bearer")
				got := <-seen
				if step < 2 {
					require.Equal(t, "Bearer owner-token", got.auth)
				} else {
					require.Equal(t, "Bearer target-token", got.auth)
				}
				if step == 4 && !ws {
					require.Equal(t, "real-"+got.auth, got.header)
					require.Empty(t, got.metadata) // compact does not accept client_metadata
				} else if step == 1 || step == 4 {
					require.Equal(t, "real-"+got.auth, got.metadata)
				} else {
					require.Empty(t, got.header)
					require.Empty(t, got.metadata)
				}
				require.False(t, h.db.IsManagedCodexTurnStateAlias(got.header))
				require.False(t, h.db.IsManagedCodexTurnStateAlias(got.metadata))
				if step == 0 {
					aliasA = returned
				}
				if step == 2 {
					aliasB = returned
					require.NotEqual(t, aliasA, aliasB)
				}
				if step < 2 {
					require.Equal(t, aliasA, returned)
				} else {
					require.Equal(t, aliasB, returned)
				}
			}
		})
	}
}

func aliasRequest(t *testing.T, h *Handler, user int64, turn, token string, ws bool) (*gin.Context, []byte, requestSessionIdentity) {
	c, _ := newTurnStateTestContext(t)
	c.Set(contextAPIKeyID, user)
	identity := requestSessionIdentity{affinityID: "alias-session", stableIdentity: true, hasDownstreamAffinity: true, hasRequestFingerprint: true}
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"hello"}],"client_metadata":{"session_id":"alias-session","thread_id":"alias-thread","x-codex-turn-metadata":{"thread_id":"alias-thread","turn_id":"placeholder"}}}`)
	body, err := sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", turn)
	require.NoError(t, err)
	if token != "" {
		body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", token)
		require.NoError(t, err)
		c.Request.Header.Set(codexTurnStateHeader, token)
	}
	if ws {
		c.Request.Header.Set("Connection", "Upgrade")
		c.Request.Header.Set("Upgrade", "websocket")
	}
	h.bindTurnStateSession(c, body, identity)
	return c, body, identity
}

func TestTurnStateAliasRestoresAndRejectsWrongOwnerTurnGeneration(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	a := &auth.Account{DBID: 71, AccountID: "account-a"}
	b := &auth.Account{DBID: 72, AccountID: "account-b"}
	h.store.AddAccount(a)
	h.store.AddAccount(b)
	first, _, identity := aliasRequest(t, h, 101, "turn-1", "", false)
	s := turnStateSessionFrom(first.Request.Context())
	_, err := h.db.CommitSessionContinuity(context.Background(), s.rootKey, database.SessionContinuityRecord{AccountID: a.ID()})
	require.NoError(t, err)
	alias, err := s.issue(first.Request.Context(), a, "real-state-a", "response_header")
	require.NoError(t, err)
	for _, ws := range []bool{false, true} {
		request, body, _ := aliasRequest(t, h, 101, "turn-1", alias, ws)
		body = normalizeTurnStateIngress(request, body)
		require.Equal(t, "real-state-a", gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String())
		out, headers := PrepareCodexTurnStateOutbound(request.Request.Context(), a, body, request.Request.Header)
		require.Equal(t, "real-state-a", gjson.GetBytes(out, "client_metadata.x-codex-turn-state").String())
		if !ws {
			require.Equal(t, "real-state-a", headers.Get(codexTurnStateHeader))
		}
		out, headers = PrepareCodexTurnStateOutbound(request.Request.Context(), b, body, request.Request.Header)
		require.Empty(t, headers.Get(codexTurnStateHeader))
		require.False(t, gjson.GetBytes(out, "client_metadata.x-codex-turn-state").Exists())
		// A -> B -> A must also clear; account ID alone is insufficient.
		ctx := context.WithValue(request.Request.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{AccountID: a.ID(), FailoverCount: 2}})
		out, headers = PrepareCodexTurnStateOutbound(ctx, a, body, request.Request.Header)
		require.Empty(t, headers.Get(codexTurnStateHeader))
		require.False(t, gjson.GetBytes(out, "client_metadata.x-codex-turn-state").Exists())
	}
	for _, tc := range []struct {
		user        int64
		turn, token string
	}{{102, "turn-1", alias}, {101, "turn-2", alias}, {101, "turn-1", "old-real-state"}, {101, "turn-1", "c2ts_v1_" + strings.Repeat("A", 43)}} {
		request, body, _ := aliasRequest(t, h, tc.user, tc.turn, tc.token, false)
		body = normalizeTurnStateIngress(request, body)
		require.Empty(t, request.Request.Header.Get(codexTurnStateHeader))
		require.False(t, gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists())
		require.Empty(t, codexTurnContinuationToken(request.Request.Header, body))
		if tc.user != 101 || tc.turn != "turn-1" {
			log, err := json.Marshal(turnStateDiagnostic(request.Request.Context()))
			require.NoError(t, err)
			require.NotContains(t, string(log), "real-state-a")
		}
	}
	// Fresh owner lookup must discard the old alias before continuation pinning.
	key := capacityAwareSessionAffinityKey(identity, 101)
	h.store.BindSessionAffinity(key, b, "")
	// Use a fresh root for the live-only fallback case.
	state := turnStateSessionFrom(first.Request.Context())
	state.rootKey = "missing-persistent-key"
	otherAlias, err := state.issue(first.Request.Context(), a, "old-state", "response_metadata")
	require.NoError(t, err)
	request, body, _ := aliasRequest(t, h, 101, "turn-1", otherAlias, false)
	body = normalizeTurnStateIngress(request, body)
	require.Empty(t, codexTurnContinuationToken(request.Request.Header, body))
	encoded, err := json.Marshal(turnStateDiagnostic(request.Request.Context()))
	require.NoError(t, err)
	require.Contains(t, string(encoded), otherAlias)
	require.Contains(t, string(encoded), "old-state")
}

func TestTurnStateOfficialEnvelopeRestorationAndDiagnosticValues(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	a := &auth.Account{DBID: 71, AccountID: "a"}
	h.store.AddAccount(a)
	raw := bytes.Repeat([]byte{0x33}, 217)
	copy(raw[:9], []byte{0x80, 0, 0, 0, 0, 0x6a, 0xab, 0xd3, 0x27})
	real := base64.URLEncoding.EncodeToString(raw)
	first, _, _ := aliasRequest(t, h, 101, "turn", "", false)
	s := turnStateSessionFrom(first.Request.Context())
	_, err := h.db.CommitSessionContinuity(t.Context(), s.rootKey, database.SessionContinuityRecord{AccountID: a.ID()})
	require.NoError(t, err)
	alias, err := s.issue(first.Request.Context(), a, real, "response_header")
	require.NoError(t, err)
	require.Len(t, alias, len(real))
	require.NotEqual(t, real, alias)
	for _, sent := range []string{alias, real, "c2ts_v1_" + strings.Repeat("A", 43)} {
		request, body, _ := aliasRequest(t, h, 101, "turn", sent, false)
		body = normalizeTurnStateIngress(request, body)
		out, headers := PrepareCodexTurnStateOutbound(request.Request.Context(), a, body, request.Request.Header)
		attachUpstreamTrace(request, h.store)
		beginUpstreamTrace(request.Request.Context(), a, "", false)
		observer := UpstreamTransportObserver(request.Request.Context())
		observer.OutboundHTTPIdentity(headers)
		observer.ResponsesInput(out, headers, "/responses")
		usage := database.UsageLogInput{AccountID: a.ID()}
		populateUpstreamTrace(request, &usage)
		populateUsageRequestDiagnostics(request, &usage)
		require.Equal(t, sent, gjson.Get(usage.RequestDiagnostics, "turn_state.events.0.received").String())
		if sent == alias {
			require.Equal(t, real, headers.Get(codexTurnStateHeader))
			require.Equal(t, real, gjson.Get(usage.RequestDiagnostics, "turn_state.events.0.real").String())
			require.Equal(t, real, gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.http.headers.X-Codex-Turn-State").String())
			require.Equal(t, real, gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.body.client_metadata.x-codex-turn-state").String())
		} else {
			require.Empty(t, headers.Get(codexTurnStateHeader))
			require.False(t, gjson.GetBytes(out, "client_metadata.x-codex-turn-state").Exists())
			require.False(t, gjson.Get(usage.RequestDiagnostics, "turn_state.events.0.real").Exists())
		}
	}
}

func TestTurnStateProvenanceDistinguishesIdenticalEnvelopeShapes(t *testing.T) {
	h, owner, target, key := failoverTestSetup(t, true)
	record, _, err := h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: continuityTestThread})
	require.NoError(t, err)
	producer, _ := outboundEpochTestRequest(t, h, 0)
	h.attachSessionOutboundEpoch(producer, hashRiskIdentity(key), record)
	raw := bytes.Repeat([]byte{0x33}, 217)
	raw[0] = 0x80
	real := base64.URLEncoding.EncodeToString(raw)
	alias, err := h.db.IssueCodexTurnState(t.Context(), database.CodexTurnStateBinding{Scope: "scope", AccountID: target.ID()}, real)
	require.NoError(t, err)
	// Epoch-only contexts and fully bound requests must both distinguish them.
	for _, bound := range []bool{false, true} {
		ctx := producer.Request.Context()
		if bound {
			ctx = context.WithValue(ctx, turnStateSessionKey{}, &turnStateSession{handler: h})
		}
		recordSessionTurnState(ctx, target, real)
		recordSessionTurnState(ctx, target, alias.Alias)
	}
	epoch := outboundEpochFromContext(producer.Request.Context())
	scope := sessionContextScope(epoch.owner, epoch.key, epoch.upstreamAccount, epoch.record)
	known, cancel := h.sessionContextVerifierForScope(t.Context(), scope)
	defer cancel()
	require.True(t, known("turn_state", real))
	require.False(t, known("turn_state", alias.Alias))
}

func TestTurnStateLargeDiagnosticRemainsBounded(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	c, _, _ := aliasRequest(t, h, 101, "turn", "", false)
	s := turnStateSessionFrom(c.Request.Context())
	for i := 0; i < 24; i++ {
		s.log("restored", "request_header", strings.Repeat("alias", 1000), strings.Repeat("real", 1000), 71, 0, nil)
	}
	var usage database.UsageLogInput
	populateUsageRequestDiagnostics(c, &usage)
	require.NotEmpty(t, usage.RequestDiagnostics)
	require.LessOrEqual(t, len(usage.RequestDiagnostics), database.MaxUsageRequestDiagnosticsBytes)
	require.True(t, gjson.Get(usage.RequestDiagnostics, "truncated").Bool())
	require.True(t, gjson.Get(usage.RequestDiagnostics, "turn_state.events.0.value_truncated").Bool())
	require.Contains(t, usage.RequestDiagnostics, "[truncated]")
	require.NotEmpty(t, gjson.Get(usage.RequestDiagnostics, "turn_state.events.0.real_hash").String())
}

func TestTurnStateAliasStreamMasksHeadersAndMultilineMetadata(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	a := &auth.Account{DBID: 71, AccountID: "a"}
	h.store.AddAccount(a)
	c, _, _ := aliasRequest(t, h, 101, "turn", "", false)
	delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	frame := "event: response.metadata\r\nid: 1\r\ndata: {\"type\":\"response.metadata\",\r\ndata: \"headers\":{\"X-Codex-Turn-State\":\"secret-real\",\"other\":\"keep\"}}\r\n\r\n"
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}, codexTurnStateHeader: []string{"secret-real"}}, Body: io.NopCloser(strings.NewReader(": keepalive\n\n" + frame + delta))}
	require.NoError(t, maskTurnStateResponse(c.Request.Context(), a, response))
	alias := response.Header.Get(codexTurnStateHeader)
	require.True(t, database.ValidCodexTurnStateAlias(alias))
	// Tiny reads simulate arbitrary TCP fragmentation, without a separate goroutine.
	var output strings.Builder
	buffer := make([]byte, 3)
	for {
		n, err := response.Body.Read(buffer)
		output.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}
	require.NotContains(t, output.String(), "secret-real")
	require.Contains(t, output.String(), alias)
	require.Contains(t, output.String(), delta)
	require.Contains(t, output.String(), "event: response.metadata\r\nid: 1\r\n")
	require.Contains(t, output.String(), `"other":"keep"`)
	logs, err := json.Marshal(turnStateDiagnostic(c.Request.Context()))
	require.NoError(t, err)
	require.Contains(t, string(logs), alias)
	require.Contains(t, string(logs), "secret-real")
	var input database.UsageLogInput
	populateUsageRequestDiagnostics(c, &input)
	require.Contains(t, input.RequestDiagnostics, alias)
	require.Contains(t, input.RequestDiagnostics, "secret-real")
}

func TestTurnStateAliasNoTokenAndFailedPersistence(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	a := &auth.Account{DBID: 71, AccountID: "a"}
	c, _, _ := aliasRequest(t, h, 101, "turn", "", false)
	response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[]}`))}
	require.NoError(t, maskTurnStateResponse(c.Request.Context(), a, response))
	require.Empty(t, response.Header.Get(codexTurnStateHeader))
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	response = &http.Response{Header: http.Header{codexTurnStateHeader: []string{"must-not-leak"}}, Body: io.NopCloser(strings.NewReader(""))}
	var requestErr error
	finishTurnStateResponse(ctx, a, &response, &requestErr)
	require.Error(t, requestErr)
	require.Nil(t, response)
	response = &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":\"must-not-leak\"}}\n\n"))}
	require.NoError(t, maskTurnStateResponse(ctx, a, response))
	data, err := io.ReadAll(response.Body)
	require.Error(t, err)
	require.NotContains(t, string(data), "must-not-leak")
}

func TestTurnStateAliasRelatedRequestsUsePersistentRoot(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	account := &auth.Account{DBID: 71, AccountID: "a"}
	h.store.AddAccount(account)
	c, body, identity := aliasRequest(t, h, 101, "child-turn", "", false)
	identity.relatedToRoot, identity.protectedRelatedLease = true, true
	h.bindTurnStateSession(c, body, identity)
	s := turnStateSessionFrom(c.Request.Context())
	require.Equal(t, hashRiskIdentity(sessionAffinityKey(identity.affinityID, 101)), s.rootKey)
	_, err := h.db.CommitSessionContinuity(t.Context(), s.rootKey, database.SessionContinuityRecord{AccountID: account.ID(), FailoverCount: 1})
	require.NoError(t, err)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{AccountID: account.ID(), FailoverCount: 1}}))
	alias, err := s.issue(c.Request.Context(), account, "child-real", "response_metadata")
	require.NoError(t, err)
	next, nextBody, _ := aliasRequest(t, h, 101, "child-turn", alias, false)
	h.bindTurnStateSession(next, nextBody, identity)
	nextBody = normalizeTurnStateIngress(next, nextBody)
	require.Equal(t, "child-real", gjson.GetBytes(nextBody, "client_metadata.x-codex-turn-state").String())
	// A sibling thread cannot consume the child's alias even under the same root.
	sibling, otherBody, _ := aliasRequest(t, h, 101, "child-turn", alias, false)
	otherBody, _ = sjson.SetBytes(otherBody, "client_metadata.x-codex-turn-metadata.thread_id", "other-child")
	h.bindTurnStateSession(sibling, otherBody, identity)
	otherBody = normalizeTurnStateIngress(sibling, otherBody)
	require.False(t, gjson.GetBytes(otherBody, "client_metadata.x-codex-turn-state").Exists())
}

func TestTurnStateAliasSSEEscapesLongLinesAndUntouchedEvents(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	account := &auth.Account{DBID: 71, AccountID: "a"}
	c, _, _ := aliasRequest(t, h, 101, "turn", "", false)
	stream := &turnStateStream{state: turnStateSessionFrom(c.Request.Context()), ctx: c.Request.Context(), account: account}
	for _, frame := range []string{
		`data: {"type":"response.metadata","headers":{"x\u002dcodex-turn-state":"secret"}}` + "\n\n",
		`data: {"type":"response.\u006detadata","headers":{"X-CODEX-TURN-STATE":["secret"]}}` + "\r\n\r\n",
	} {
		masked, err := stream.maskFrame([]byte(frame))
		require.NoError(t, err)
		require.NotContains(t, string(masked), "secret")
		require.Contains(t, string(masked), "gAAAA")
	}
	for _, frame := range []string{"data: {\"type\":\"response.metadata\",\"other\":1}\n\n", ": ping\n\n", "data: [DONE]\n\n"} {
		actual, err := stream.maskFrame([]byte(frame))
		require.NoError(t, err)
		require.Equal(t, frame, string(actual))
	}
	long := `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 65536) + `"}` + "\n\n"
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(long))}
	require.NoError(t, maskTurnStateResponse(c.Request.Context(), account, response))
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, long, string(data))
}

func BenchmarkTurnStateSSETextDelta(b *testing.B) {
	frame := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"some ordinary text\"}\n\n")
	r := &turnStateStream{}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = r.maskFrame(frame)
	}
}
