package proxy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

var toolErrorCredentials = regexp.MustCompile(`(?i)(bearer\s+|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|cookie|password|secret)\s*[:=]\s*)[^\s,;"}]+`)

// Preserve the tool's error shape and actionable prose, while removing gateway
// identity extensions and known account credentials. Content is tool data;
// names such as user/session_id inside that content are not protocol fields.
func (w responsePrivacyWalker) toolError(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var secrets []string
	if w.account != nil {
		w.account.Mu().RLock()
		secrets = append(secrets, w.account.AccessToken, w.account.RefreshToken, w.account.SessionToken, w.account.AccountID, w.account.Email)
		w.account.Mu().RUnlock()
		_, credential := w.account.OpenAIResponsesCredentials()
		secrets = append(secrets, credential)
	}
	if d, _ := w.ctx.Value(codexAccountIdentityDiagnosticKey{}).(*codexAccountIdentityDiagnostic); d != nil {
		for _, change := range d.Changes {
			secrets = append(secrets, change.Outbound)
		}
	}
	// The WS executor derives a child context, while its HTTP/SSE adapter is
	// read under the parent's context. Supplement redaction from the same
	// attempt's shared snapshot; this is never used to authorize restoration.
	if audit := upstreamTraceFromContext(w.ctx); audit != nil && w.account != nil {
		audit.mu.Lock()
		if attempt := audit.current; attempt != nil && attempt.accountID == w.account.ID() && attempt.transport.OutboundIdentity != nil {
			identity := attempt.transport.OutboundIdentity
			if mapping := identity.AccountMapping; mapping != nil {
				for _, change := range mapping.Changes {
					secrets = append(secrets, change.Outbound)
				}
			}
			for _, carrier := range []*OutboundHeaderDiagnostic{identity.HTTP, identity.WSHandshake} {
				if carrier != nil {
					for key, value := range carrier.Headers {
						if privateResponseField(key) || isTurnStateField(key) {
							secrets = append(secrets, value)
						}
					}
				}
			}
		}
		audit.mu.Unlock()
	}
	var walk func(any, bool, int) (any, error)
	walk = func(v any, business bool, depth int) (any, error) {
		if depth > 64 {
			return nil, nil
		}
		switch x := v.(type) {
		case string:
			for _, secret := range secrets {
				if len(secret) >= 8 {
					x = strings.ReplaceAll(x, secret, "[redacted]")
				}
			}
			x = toolErrorCredentials.ReplaceAllString(x, "${1}[redacted]")
			out, err := w.errorText(x)
			if err != nil {
				return nil, err
			}
			var text string
			_ = json.Unmarshal(out, &text)
			return text, nil
		case []any:
			for i := range x {
				out, err := walk(x[i], business, depth+1)
				if err != nil {
					return nil, err
				}
				x[i] = out
			}
		case map[string]any:
			for key, child := range x {
				if !business && (privateResponseField(key) || isTurnStateField(key) || privacyField(key) == "headers" || privacyField(key) == "streamid") {
					delete(x, key)
					continue
				}
				out, err := walk(child, business || key == "content", depth+1)
				if err != nil {
					return nil, err
				}
				x[key] = out
			}
		}
		return v, nil
	}
	out, err := walk(value, false, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}
