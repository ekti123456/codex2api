package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

var errTurnStateMapping = errors.New("turn-state mapping unavailable")

func finishTurnStateResponse(ctx context.Context, account *auth.Account, response **http.Response, requestErr *error) {
	if *response == nil {
		return
	}
	if err := maskTurnStateResponse(ctx, account, *response); err != nil {
		if (*response).Body != nil {
			_ = (*response).Body.Close()
		}
		*response = nil
		*requestErr = ErrInternalError("会话状态暂时无法处理，请稍后重试。", errTurnStateMapping)
	}
}

// This is applied to the shared HTTP response abstraction, including WS upstream
// frames converted to SSE. It never invents a metadata event or token.
func maskTurnStateResponse(ctx context.Context, account *auth.Account, response *http.Response) error {
	s := turnStateSessionFrom(ctx)
	if response == nil || s == nil {
		return nil
	}
	observeUsageTurnState(ctx, "")
	response.Header = response.Header.Clone()
	var real string
	for key, values := range response.Header {
		if strings.EqualFold(key, codexTurnStateHeader) && len(values) > 0 {
			real = values[0]
			break
		}
	}
	deleteTurnStateHeader(response.Header)
	if real != "" {
		alias, err := s.issue(ctx, account, real, "response_header")
		if err != nil {
			return err
		}
		if alias != "" {
			response.Header.Set(codexTurnStateHeader, alias)
		}
	}
	if response.Body != nil && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body = &turnStateStream{body: response.Body, reader: bufio.NewReaderSize(response.Body, 32*1024), ctx: ctx, account: account, state: s}
		response.ContentLength = -1
		response.Header.Del("Content-Length")
	}
	return nil
}

// stageTurnStateMetadataHeader promotes the first masked metadata token into
// this attempt's response headers. Codex's HTTP client reads turn state from
// HTTP headers, not from SSE metadata. Keep this on the handler goroutine:
// body reads can run in a separate goroutine while retry heartbeats are sent.
// The downstream header is published only when this attempt is actually used.
func stageTurnStateMetadataHeader(ctx context.Context, headers http.Header, event gjson.Result) {
	if headers == nil || headers.Get(codexTurnStateHeader) != "" {
		return
	}
	switch strings.TrimSpace(event.Get("type").String()) {
	case "response.metadata", "codex.response.metadata", "responsesapi.response.metadata":
	default:
		return
	}
	s := turnStateSessionFrom(ctx)
	if s == nil || s.handler == nil || s.handler.db == nil {
		return
	}
	event.Get("headers").ForEach(func(key, value gjson.Result) bool {
		if !strings.EqualFold(key.String(), codexTurnStateHeader) {
			return true
		}
		if value.IsArray() {
			values := value.Array()
			if len(values) != 1 {
				return true
			}
			value = values[0]
		}
		// Only the alias already issued by the masking layer may be promoted.
		// Never expose raw upstream state through this new response-header path.
		if value.Type == gjson.String && s.handler.db.IsManagedCodexTurnStateAlias(value.String()) {
			headers.Set(codexTurnStateHeader, value.String())
			return false
		}
		return true
	})
}

// Buffer one SSE event, not the response. Parse response.metadata and its
// transport-prefixed variants; keep other events byte-for-byte, including
// comments, ids and multiline data fields.
// The limit also bounds malformed streams without an event separator.
type turnStateStream struct {
	body     io.ReadCloser
	reader   *bufio.Reader
	ctx      context.Context
	account  *auth.Account
	state    *turnStateSession
	pending  []byte
	terminal error
}

func (r *turnStateStream) Close() error { return r.body.Close() }
func (r *turnStateStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 && r.terminal == nil {
		var frame []byte
		lineContinued := false
		for {
			line, err := r.reader.ReadSlice('\n')
			if len(frame)+len(line) > 16<<20 {
				r.terminal = errors.New("upstream SSE event too large")
				break
			}
			frame = append(frame, line...)
			if err == bufio.ErrBufferFull {
				lineContinued = true
				continue
			}
			if err != nil {
				r.terminal = err
				break
			}
			if !lineContinued && (bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))) {
				break
			}
			lineContinued = false
		}
		if r.terminal != nil && r.terminal != io.EOF {
			return 0, r.terminal
		}
		if len(frame) > 0 {
			var err error
			r.pending, err = r.maskFrame(frame)
			if err != nil {
				r.pending = nil
				r.terminal = err
				return 0, err
			}
		}
	}
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	return 0, r.terminal
}

func (r *turnStateStream) maskFrame(frame []byte) ([]byte, error) {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	var data []byte
	multiline := false
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			part := bytes.TrimSuffix(bytes.TrimSuffix(line[5:], []byte("\n")), []byte("\r"))
			part = bytes.TrimPrefix(part, []byte(" "))
			if data == nil {
				data = part
				continue
			}
			if !multiline {
				data = bytes.Clone(data)
				multiline = true
			}
			data = append(data, '\n')
			data = append(data, part...)
		}
	}
	// Parse only the event discriminator on the common path. JSON escapes in
	// either the discriminator or header name must not bypass token masking.
	switch strings.TrimSpace(gjson.GetBytes(data, "type").String()) {
	case "response.metadata", "codex.response.metadata", "responsesapi.response.metadata":
	default:
		return frame, nil
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, errors.New("invalid upstream turn-state event")
	}
	var headers map[string]json.RawMessage
	if len(event["headers"]) == 0 || string(event["headers"]) == "null" {
		return frame, nil
	}
	if err := json.Unmarshal(event["headers"], &headers); err != nil {
		return nil, errors.New("invalid upstream turn-state headers")
	}
	for key, raw := range headers {
		if !strings.EqualFold(key, codexTurnStateHeader) {
			continue
		}
		var real string
		array := false
		if err := json.Unmarshal(raw, &real); err != nil {
			var values []string
			if json.Unmarshal(raw, &values) != nil || len(values) != 1 {
				delete(headers, key)
				continue
			}
			real, array = values[0], true
		}
		alias, err := r.state.issue(r.ctx, r.account, real, "response_metadata")
		if err != nil {
			return nil, err
		}
		if alias == "" {
			delete(headers, key)
			continue
		}
		if array {
			headers[key], _ = json.Marshal([]string{alias})
		} else {
			headers[key], _ = json.Marshal(alias)
		}
	}
	event["headers"], _ = json.Marshal(headers)
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	var output []byte
	replaced := false
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if replaced {
				continue
			}
			output = append(output, []byte("data: ")...)
			output = append(output, encoded...)
			if bytes.HasSuffix(line, []byte("\r\n")) {
				output = append(output, '\r', '\n')
			} else if bytes.HasSuffix(line, []byte("\n")) {
				output = append(output, '\n')
			}
			replaced = true
		} else {
			output = append(output, line...)
		}
	}
	return output, nil
}
