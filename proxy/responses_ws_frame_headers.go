package proxy

import "net/http"

func codexWebsocketCurrentFrameHeaders(headers http.Header, body []byte) http.Header {
	current := headers.Clone()
	for _, name := range []string{
		"Session-Id", "Session_id", "Thread-Id", "Conversation-Id", "Conversation_id",
		"X-Client-Request-Id", "X-Codex-Window-Id", "X-Codex-Turn-Metadata", "X-Codex-Turn-State",
		"X-Codex-Context-Window-Id",
		"X-Codex-Parent-Thread-Id", "X-Codex-Forked-From-Thread-Id", "X-OpenAI-Subagent", "X-OpenAI-Memgen-Request",
	} {
		current.Del(name)
	}
	return CodexRequestMetadataHeaders(current, body)
}
