package proxy

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// These are client-reported labels, not a gateway inference about the client's
// implementation. Keep carriers separate so conflicts are visible in the log.
type compactionMetadataDiagnostic struct {
	Header compactionMetadataValueDiagnostic `json:"header"`
	Body   compactionMetadataValueDiagnostic `json:"body"`
}

type compactionMetadataValueDiagnostic struct {
	State          string   `json:"state"`
	Implementation string   `json:"implementation,omitempty"`
	Trigger        string   `json:"trigger,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Phase          string   `json:"phase,omitempty"`
	Strategy       string   `json:"strategy,omitempty"`
	InvalidFields  []string `json:"invalid_fields,omitempty"`
}

func captureCompactionMetadata(root gjson.Result, headers http.Header) *compactionMetadataDiagnostic {
	header := compactionMetadataValueDiagnostic{State: "absent"}
	if raw := headers.Get(codexTurnMetadataHeader); strings.TrimSpace(raw) != "" {
		if len(raw) > 16384 {
			header.State = "too_large"
		} else if !gjson.Valid(raw) {
			header.State = "invalid_metadata"
		} else {
			header = captureCompactionMetadataValue(gjson.Parse(raw))
		}
	}
	return &compactionMetadataDiagnostic{
		Header: header,
		Body:   captureCompactionMetadataValue(root.Get("client_metadata.x-codex-turn-metadata")),
	}
}

func captureCompactionMetadataValue(metadata gjson.Result) compactionMetadataValueDiagnostic {
	result := compactionMetadataValueDiagnostic{State: "absent"}
	if !metadata.Exists() {
		return result
	}
	if len(metadata.Raw) > 16384 {
		result.State = "too_large"
		return result
	}
	if metadata.Type == gjson.String {
		if !gjson.Valid(metadata.String()) {
			result.State = "invalid_metadata"
			return result
		}
		metadata = gjson.Parse(metadata.String())
	}
	if !metadata.IsObject() {
		result.State = "invalid_metadata"
		return result
	}
	value := metadata.Get("compaction")
	if !value.Exists() {
		return result
	}
	if !value.IsObject() {
		result.State = "invalid_type"
		return result
	}
	if len(value.Raw) > 2048 {
		result.State = "too_large"
		return result
	}
	result.State = "present"
	for _, field := range []struct {
		name   string
		target *string
	}{
		{"implementation", &result.Implementation}, {"trigger", &result.Trigger},
		{"reason", &result.Reason}, {"phase", &result.Phase}, {"strategy", &result.Strategy},
	} {
		label := value.Get(field.name)
		if !label.Exists() {
			continue
		}
		// Bound values and reject free text; never persist arbitrary metadata.
		if label.Type != gjson.String || !sessionErrorLabel(label.String()) {
			result.InvalidFields = append(result.InvalidFields, field.name)
			continue
		}
		*field.target = strings.Clone(label.String())
	}
	return result
}
