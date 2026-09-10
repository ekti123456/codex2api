package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

func ResolveCodexOutboundClientIdentity(account *auth.Account, apiKey string, deviceCfg *DeviceProfileConfig, downstreamHeaders http.Header) (userAgent, version, originator string) {
	userAgent, version, generated := resolveCodexOutboundClientHeaders(account, apiKey, deviceCfg, downstreamHeaders)
	originator = Originator
	if generated {
		originator = CodexOriginatorForGeneratedUserAgent(userAgent)
	} else if incoming := strings.TrimSpace(downstreamHeaders.Get("Originator")); incoming != "" && IsCodexOfficialClientByHeaders("", incoming) {
		originator = incoming
	}
	return userAgent, version, originator
}

func applyCodexAuxiliaryClientHeaders(request *http.Request, account *auth.Account, apiKey string, deviceCfg *DeviceProfileConfig, downstreamHeaders http.Header, requestedVersion string) {
	if request == nil {
		return
	}
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	userAgent, version, originator := ResolveCodexOutboundClientIdentity(account, apiKey, deviceCfg, downstreamHeaders)
	if requestedVersion = strings.TrimSpace(requestedVersion); requestedVersion != "" {
		userAgent = replaceCodexUserAgentVersion(userAgent, requestedVersion)
		version = requestedVersion
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Version", version)
	request.Header.Set("Originator", originator)
	if account != nil {
		for name, value := range account.GetCustomHeaders() {
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "user-agent", "version", "originator":
				request.Header.Set(strings.TrimSpace(name), value)
			}
		}
	}
	RecordUpstreamUserAgent(request.Context(), request.Header.Get("User-Agent"))
}
