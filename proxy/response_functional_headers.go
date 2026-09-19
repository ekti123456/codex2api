package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type codexManifestSignal struct {
	versions map[string]string
	updated  time.Time
}

func codexManifestSignalScope(c *gin.Context) string {
	var policy []byte
	if row := apiKeyRowFromContext(c); row != nil {
		// QuotaUsed/TotalUsed change on every request and are not catalogue
		// identity. Including them would cause a refresh loop after billing.
		policy, _ = json.Marshal(map[string]any{"groups": row.AllowedGroupIDs, "limits": row.Limits})
	}
	return codexIdentityDigest("visible-models-v1", fmt.Sprint(requestAPIKeyID(c)), string(policy), requestUpstreamChannel(c))
}

// A response's upstream validator is only an invalidation signal. Once the
// client refreshes /models, subsequent responses advertise that visible
// catalogue's ETag (including filtering and locally merged models).
func (h *Handler) manifestSignal(c *gin.Context, upstream, published string) string {
	if h == nil {
		return ""
	}
	h.manifestSignalsMu.Lock()
	defer h.manifestSignalsMu.Unlock()
	if h.manifestSignals == nil {
		h.manifestSignals = make(map[string]*codexManifestSignal)
	}
	now := time.Now()
	scope := codexManifestSignalScope(c)
	entry := h.manifestSignals[scope]
	if entry == nil && len(h.manifestSignals) >= 4096 {
		for key, value := range h.manifestSignals {
			if now.Sub(value.updated) > time.Hour {
				delete(h.manifestSignals, key)
			}
		}
		if len(h.manifestSignals) >= 4096 {
			return ""
		}
	}
	if entry == nil {
		entry = &codexManifestSignal{versions: make(map[string]string)}
		h.manifestSignals[scope] = entry
	}
	entry.updated = now
	if published != "" {
		for version := range entry.versions {
			entry.versions[version] = published
		}
		return published
	}
	if upstream == "" {
		return ""
	}
	version := codexIdentityDigest("model-version", scope, upstream)
	if tag := entry.versions[version]; tag != "" {
		return tag
	}
	if len(entry.versions) >= 32 {
		entry.versions = make(map[string]string)
	}
	entry.versions[version] = ""
	return `"refresh-` + version[:24] + `"`
}

func clearFunctionalResponseHeaders(headers http.Header) {
	for _, name := range []string{"X-Reasoning-Included", "OpenAI-Model", "X-Models-Etag"} {
		headers.Del(name)
	}
}

func relayFunctionalResponseHeaders(c *gin.Context, upstream http.Header) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	clearFunctionalResponseHeaders(c.Writer.Header())
	// Codex checks presence, including an empty header value. Never turn a
	// missing header into 'false': its presence would mean the opposite.
	if _, ok := upstream[http.CanonicalHeaderKey("X-Reasoning-Included")]; ok {
		c.Writer.Header()["X-Reasoning-Included"] = []string{upstream.Get("X-Reasoning-Included")}
	}
	if model := upstream.Get("OpenAI-Model"); model != "" {
		c.Header("OpenAI-Model", model)
	}
	if upstream.Get("X-Models-Etag") != "" {
		if s, _ := c.Request.Context().Value(protocolIdentityKey{}).(*responseIdentitySession); s != nil && s.handler != nil {
			if tag := s.handler.manifestSignal(c, upstream.Get("X-Models-Etag"), ""); tag != "" {
				c.Header("X-Models-Etag", tag)
			}
		}
	}
}
