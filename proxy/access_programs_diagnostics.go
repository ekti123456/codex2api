package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/tidwall/gjson"
)

// Capture only this parameter, not the surrounding prompt. Oversized values
// retain a fingerprint instead of consuming the shared diagnostic budget.
const maxAccessProgramsDiagnosticBytes = 1024

type accessProgramsValueDiagnostic struct {
	State  string          `json:"state"`
	Value  json.RawMessage `json:"value,omitempty"`
	Bytes  int             `json:"bytes,omitempty"`
	SHA256 string          `json:"sha256,omitempty"`
}

type accessProgramsDiagnostic struct {
	Inbound  *accessProgramsValueDiagnostic `json:"inbound,omitempty"`
	Outbound *accessProgramsValueDiagnostic `json:"outbound,omitempty"`
}

func captureAccessPrograms(body []byte) *accessProgramsValueDiagnostic {
	if !gjson.ValidBytes(body) {
		return &accessProgramsValueDiagnostic{State: "invalid_json"}
	}
	field := gjson.GetBytes(body, "access_programs")
	if !field.Exists() {
		return &accessProgramsValueDiagnostic{State: "absent"}
	}
	var value json.RawMessage
	if len(field.Raw) <= maxAccessProgramsDiagnosticBytes {
		// Bound the stored JSON as well: escaping '<' and similar characters
		// can expand a small wire value several-fold during log serialization.
		value, _ = json.Marshal(json.RawMessage(field.Raw))
	}
	if value == nil || len(value) > maxAccessProgramsDiagnosticBytes {
		digest := sha256.Sum256([]byte(field.Raw))
		return &accessProgramsValueDiagnostic{State: "too_large", Bytes: len(field.Raw), SHA256: hex.EncodeToString(digest[:])}
	}
	// RawMessage keeps null, empty objects, strings and numeric types distinct.
	// Copy the small field so snapshots never retain or alias the full request.
	return &accessProgramsValueDiagnostic{State: "present", Value: value}
}
