package database

import "encoding/json"

type UsageLogExportEntry struct {
	*UsageLog
	Diagnostics json.RawMessage `json:"diagnostics"`
}

func usageExportDiagnosticJSON(payload string) json.RawMessage {
	if payload == "" {
		return nil
	}
	if !json.Valid([]byte(payload)) {
		return json.RawMessage(`{"capture_status":"invalid_stored_json"}`)
	}
	return json.RawMessage(payload)
}
