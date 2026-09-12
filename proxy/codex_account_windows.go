package proxy

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type codexAccountWindowChange struct {
	ThreadID string `json:"thread_id"`
	Original uint64 `json:"original_number"`
	Base     uint64 `json:"base_number"`
	Outbound uint64 `json:"outbound_number"`
}

func parseAccountWindow(value string) (string, uint64, error) {
	separator := strings.LastIndexByte(value, ':')
	if separator <= 0 {
		return "", 0, errors.New("invalid outbound window ID")
	}
	thread := normalizeSessionGraphValue(value[:separator])
	number, err := strconv.ParseUint(value[separator+1:], 10, 64)
	if err != nil || !validSessionGraphUUID(thread) {
		return "", 0, errors.New("invalid outbound window ID")
	}
	return thread, number, nil
}

func accountMetadataThread(source gjson.Result) string {
	for _, field := range []string{"window_id", "x-codex-window-id", "x_codex_window_id"} {
		if thread, _, err := parseAccountWindow(source.Get(field).String()); err == nil {
			return thread
		}
	}
	if thread := source.Get("thread_id").String(); thread != "" {
		return thread
	}
	return source.Get("session_id").String()
}

func codexAccountWindowInputs(headers http.Header, body []byte) (map[string]uint64, error) {
	windows := make(map[string]uint64)
	metadata := gjson.GetBytes(body, "client_metadata")
	for index, source := range []gjson.Result{metadata, diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata")), gjson.Parse(headers.Get(codexTurnMetadataHeader))} {
		thread := source.Get("thread_id").String()
		if thread == "" {
			thread = accountMetadataThread(source)
		}
		if thread == "" {
			switch index {
			case 0:
				thread = accountMetadataThread(diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata")))
			case 1:
				thread = accountMetadataThread(metadata)
			case 2:
				thread = headers.Get(codexThreadIDHeader)
			}
		}
		for _, field := range []string{"window_id", "x-codex-window-id", "x_codex_window_id"} {
			if value := source.Get(field); value.Exists() {
				windowThread, number, err := parseAccountWindow(value.String())
				if err != nil || thread != "" && !strings.EqualFold(thread, windowThread) {
					return nil, errors.New("outbound window thread is inconsistent")
				}
				thread = windowThread
				if previous, exists := windows[thread]; exists && previous != number {
					return nil, errors.New("outbound window numbers are inconsistent")
				}
				windows[thread] = number
			}
		}
		if number := source.Get("window_number"); number.Exists() {
			parsed, err := strconv.ParseUint(number.Raw, 10, 64)
			if err != nil || number.Type != gjson.Number || !validSessionGraphUUID(thread) {
				return nil, errors.New("invalid outbound window number")
			}
			thread = strings.ToLower(thread)
			if previous, exists := windows[thread]; exists && previous != parsed {
				return nil, errors.New("outbound window numbers are inconsistent")
			}
			windows[thread] = parsed
		}
	}
	if value := headers.Get(codexWindowIDHeader); value != "" {
		thread, number, err := parseAccountWindow(value)
		if err != nil {
			return nil, err
		}
		if previous, exists := windows[thread]; exists && previous != number {
			return nil, errors.New("outbound window numbers are inconsistent")
		}
		windows[thread] = number
	}
	return windows, nil
}

func (fingerprint *CodexFingerprint) prepareAccountWindows(ctx context.Context, mapping *codexAccountIdentity, epoch *sessionOutboundEpoch) error {
	if epoch == nil || !epoch.record.OutboundWindowReset {
		return nil
	}
	if fingerprint.accountWindowInputError != nil {
		return codexAccountIdentityError("出站窗口序号不一致，无法按迁移段重新编号。")
	}
	bases := epoch.record.OutboundWindowBases
	if !epoch.preview {
		var err error
		bases, err = epoch.handler.db.ResolveSessionOutboundWindows(ctx, epoch.key, epoch.record.AccountID, epoch.record.FailoverCount, fingerprint.accountWindowInputs)
		if err != nil {
			return codexAccountIdentityError("无法确认当前账号的窗口起始序号，或请求早于本次迁移，请重新发起请求。")
		}
	}
	mapping.windowBases = make(map[string]uint64)
	for thread, number := range fingerprint.accountWindowInputs {
		base, exists := bases[thread]
		if !exists || number < base {
			return codexAccountIdentityError("请求窗口早于当前账号迁移段，已停止发送。")
		}
		mapping.windowBases[thread] = base
		mapping.diagnostic.Windows = append(mapping.diagnostic.Windows, codexAccountWindowChange{ThreadID: thread, Original: number, Base: base, Outbound: number - base})
	}
	return nil
}
