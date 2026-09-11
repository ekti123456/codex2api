package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCompactionTriggerNormalizationPreservesFields(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		input string
	}{
		{"move", `[{"type":"compaction_trigger","extension":{"value":42}},{"role":"user","content":"continue"}]`},
		{"canonicalize", `[{"type":" COMPACTION_TRIGGER ","extension":{"value":42}}]`},
		{"object", `{"type":"compaction_trigger","extension":{"value":42}}`},
		{"duplicates_keep_last", `[{"type":"compaction_trigger","extension":{"value":1}},{"type":"compaction_trigger","extension":{"value":42}},{"role":"user","content":"continue"}]`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			body := []byte(`{"model":"gpt-5.6-sol","input":` + scenario.input + `}`)
			for _, addIfMissing := range []bool{false, true} {
				prepared := normalizeCompactionTriggerFinal(body, addIfMissing)
				items := gjson.GetBytes(prepared, "input").Array()
				require.NotEmpty(test, items)
				trigger := items[len(items)-1]
				require.Equal(test, "compaction_trigger", trigger.Get("type").String())
				require.EqualValues(test, 42, trigger.Get("extension.value").Int())
				require.Equal(test, string(prepared), string(normalizeCompactionTriggerFinal(prepared, addIfMissing)))
			}
		})
	}
}

func TestCompactionPreparationMetadataDoesNotInjectTrigger(test *testing.T) {
	for _, prepare := range []func([]byte) ([]byte, string){PrepareResponsesBody, PrepareResponsesWebSocketBody} {
		for _, metadata := range []string{`{"request_kind":"compaction"}`, `"{\"request_kind\":\"compaction\"}"`} {
			body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":` + metadata + `},"input":[{"role":"user","content":"Summarize this conversation."}]}`)
			prepared, _ := prepare(body)
			require.False(test, requestBodyHasCompactionTrigger(prepared))
			require.Equal(test, "Summarize this conversation.", gjson.GetBytes(prepared, "input.0.content").String())
		}
	}
}

func TestCompactionPreparationPreservesTriggerFieldsAndEncryptedPayload(test *testing.T) {
	for _, prepare := range []func([]byte) ([]byte, string){PrepareResponsesBody, PrepareResponsesWebSocketBody} {
		body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"compaction_trigger","extension":{"value":42}},{"type":"compaction","id":"cmp_test","encrypted_content":"opaque-test-payload"},{"role":"user","content":"continue"}]}`)
		prepared, _ := prepare(body)
		require.Equal(test, "cmp_test", gjson.GetBytes(prepared, "input.0.id").String())
		require.Equal(test, "opaque-test-payload", gjson.GetBytes(prepared, "input.0.encrypted_content").String())
		require.Equal(test, "compaction_trigger", gjson.GetBytes(prepared, "input.2.type").String())
		require.EqualValues(test, 42, gjson.GetBytes(prepared, "input.2.extension.value").Int())
	}
}

func TestCompactionPreparationPreservesDurableItemIDs(test *testing.T) {
	for _, itemType := range []string{"compaction", "context_compaction", "compaction_summary"} {
		test.Run(itemType, func(test *testing.T) {
			for _, prepare := range []func([]byte) ([]byte, string){PrepareResponsesBody, PrepareResponsesWebSocketBody, PrepareCompactResponsesBody} {
				body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"` + itemType + `","id":"cmp_saved","encrypted_content":"opaque-payload","extension":{"value":42}},{"type":"message","id":"msg_old","role":"user","content":"continue"}]}`)
				prepared, expanded := prepare(body)
				for _, item := range []gjson.Result{gjson.GetBytes(prepared, "input.0"), gjson.Get(expanded, "0")} {
					require.Equal(test, "cmp_saved", item.Get("id").String())
					require.Equal(test, "opaque-payload", item.Get("encrypted_content").String())
					require.EqualValues(test, 42, item.Get("extension.value").Int())
				}
				require.False(test, gjson.GetBytes(prepared, "input.1.id").Exists())
			}
		})
	}
}

func TestCompactionCacheReplayPreservesDurableItemIDs(test *testing.T) {
	resetResponseCacheForTest()
	test.Cleanup(resetResponseCacheForTest)
	for _, itemType := range []string{"compaction", "context_compaction", "compaction_summary"} {
		input := []byte(`[{"type":"` + itemType + `","id":"cmp_saved","encrypted_content":"opaque-payload","extension":{"value":42}},{"type":"message","id":"msg_old","role":"user","content":"continue"}]`)
		completed := []byte(`{"type":"response.completed","response":{"id":"resp_compaction_cache","output":[{"type":"function_call","id":"fc_old","call_id":"call_test","name":"lookup","arguments":"{}"}]}}`)
		cacheCompletedResponse("compaction-cache-test", input, completed)
		cached := getResponseCache("compaction-cache-test", "resp_compaction_cache")
		require.Len(test, cached, 3)
		require.Equal(test, "cmp_saved", gjson.GetBytes(cached[0], "id").String())
		require.Equal(test, "opaque-payload", gjson.GetBytes(cached[0], "encrypted_content").String())
		require.EqualValues(test, 42, gjson.GetBytes(cached[0], "extension.value").Int())
		require.False(test, gjson.GetBytes(cached[1], "id").Exists())
		require.False(test, gjson.GetBytes(cached[2], "id").Exists())
	}
}
