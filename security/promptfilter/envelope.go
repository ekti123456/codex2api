package promptfilter

import (
	"strings"

	"github.com/tidwall/gjson"
)

type Protocol string

const (
	ProtocolUnknown   Protocol = "unknown"
	ProtocolResponses Protocol = "responses"
	ProtocolChat      Protocol = "chat_completions"
	ProtocolMessages  Protocol = "messages"
	ProtocolImages    Protocol = "images"
)

type Transport string

const (
	TransportHTTP      Transport = "http"
	TransportWebSocket Transport = "websocket"
)

type ModelFamily string

const (
	ModelFamilyOpenAI    ModelFamily = "openai"
	ModelFamilyAnthropic ModelFamily = "anthropic"
	ModelFamilyXAI       ModelFamily = "xai"
	ModelFamilyUnknown   ModelFamily = "unknown"
)

type SegmentOrigin string

type SegmentTrust string

const (
	OriginCurrentUser       SegmentOrigin = "current_user"
	OriginHistory           SegmentOrigin = "history"
	OriginSystem            SegmentOrigin = "system"
	OriginDeveloper         SegmentOrigin = "developer"
	OriginInstructions      SegmentOrigin = "instructions"
	OriginToolOutput        SegmentOrigin = "tool_output"
	OriginToolArguments     SegmentOrigin = "tool_arguments"
	OriginAttachmentRefs    SegmentOrigin = "attachment_refs"
	OriginSessionContext    SegmentOrigin = "session_context"
	OriginAttachmentContent SegmentOrigin = "attachment_content"
)

const (
	SegmentTrustClientSupplied SegmentTrust = "client_supplied"
	SegmentTrustGatewaySigned  SegmentTrust = "gateway_signed"
	SegmentTrustServerInjected SegmentTrust = "server_injected"
)

// Segment keeps request text and its provenance separate. Sequence reflects
// wire order within the request; Role retains the protocol role when present.
type Segment struct {
	Origin   SegmentOrigin `json:"origin"`
	Role     string        `json:"role,omitempty"`
	Text     string        `json:"text"`
	Sequence int           `json:"sequence"`
	Linked   bool          `json:"linked,omitempty"`
	Trust    SegmentTrust  `json:"trust"`
}

type RequestEnvelope struct {
	Endpoint       string      `json:"endpoint"`
	Protocol       Protocol    `json:"protocol"`
	Transport      Transport   `json:"transport"`
	RequestedModel string      `json:"requested_model,omitempty"`
	EffectiveModel string      `json:"effective_model,omitempty"`
	ModelFamily    ModelFamily `json:"model_family"`
	Segments       []Segment   `json:"segments"`
}

func BuildEnvelope(body []byte, endpoint string, requestedModel string, transport Transport, maxLen int) RequestEnvelope {
	return BuildEnvelopeWithModels(body, endpoint, requestedModel, "", transport, maxLen)
}

func BuildEnvelopeWithModels(body []byte, endpoint string, requestedModel string, effectiveModel string, transport Transport, maxLen int) RequestEnvelope {
	protocol := ProtocolForEndpoint(endpoint)
	if transport != TransportWebSocket {
		transport = TransportHTTP
	}
	if requestedModel = strings.TrimSpace(requestedModel); requestedModel == "" && gjson.ValidBytes(body) {
		requestedModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	envelope := RequestEnvelope{
		Endpoint:       strings.TrimSpace(endpoint),
		Protocol:       protocol,
		Transport:      transport,
		RequestedModel: requestedModel,
		EffectiveModel: strings.TrimSpace(effectiveModel),
		ModelFamily:    ResolveModelFamily(requestedModel, effectiveModel),
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return envelope
	}

	builder := envelopeBuilder{envelope: &envelope, maxLen: maxLen}
	switch protocol {
	case ProtocolResponses:
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "instructions"))
		builder.appendMessages(gjson.GetBytes(body, "input"))
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "prompt"))
		if !envelopeHasOrigin(envelope, OriginCurrentUser) {
			builder.appendMessages(gjson.GetBytes(body, "messages"))
		}
	case ProtocolChat:
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "instructions"))
		builder.appendMessages(gjson.GetBytes(body, "messages"))
	case ProtocolMessages:
		builder.appendResult(OriginSystem, "system", gjson.GetBytes(body, "system"))
		builder.appendMessages(gjson.GetBytes(body, "messages"))
	case ProtocolImages:
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "prompt"))
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "style"))
		builder.appendAttachmentReferences(gjson.ParseBytes(body), "user")
	default:
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "instructions"))
		builder.appendMessages(gjson.GetBytes(body, "messages"))
		builder.appendMessages(gjson.GetBytes(body, "input"))
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "prompt"))
	}
	return envelope
}

func ProtocolForEndpoint(endpoint string) Protocol {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case "response", "responses", "responses_compact", "/v1/responses", "/v1/responses/compact":
		return ProtocolResponses
	case "chat", "chat_completions", "/v1/chat/completions":
		return ProtocolChat
	case "messages", "anthropic", "/v1/messages":
		return ProtocolMessages
	case "image", "images", "images_generations", "images_edits", "/v1/images/generations", "/v1/images/edits":
		return ProtocolImages
	default:
		return ProtocolUnknown
	}
}

func ResolveModelFamily(requestedModel string, effectiveModel string) ModelFamily {
	model := strings.ToLower(strings.TrimSpace(effectiveModel))
	if model == "" {
		model = strings.ToLower(strings.TrimSpace(requestedModel))
	}
	switch {
	case strings.HasPrefix(model, "claude") || strings.Contains(model, "anthropic"):
		return ModelFamilyAnthropic
	case strings.HasPrefix(model, "grok") || strings.Contains(model, "xai") || strings.Contains(model, "x-ai"):
		return ModelFamilyXAI
	case strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "chatgpt-") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") || strings.Contains(model, "codex"):
		return ModelFamilyOpenAI
	default:
		return ModelFamilyUnknown
	}
}

func (e RequestEnvelope) SegmentsForOrigin(origin SegmentOrigin) []Segment {
	out := make([]Segment, 0)
	for _, segment := range e.Segments {
		if segment.Origin == origin {
			out = append(out, segment)
		}
	}
	return out
}

func envelopeHasOrigin(envelope RequestEnvelope, origin SegmentOrigin) bool {
	for _, segment := range envelope.Segments {
		if segment.Origin == origin {
			return true
		}
	}
	return false
}

type envelopeBuilder struct {
	envelope *RequestEnvelope
	maxLen   int
	sequence int
}

func (b *envelopeBuilder) append(origin SegmentOrigin, role string, text string) {
	if b == nil || b.envelope == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	text = limitScanText(text, b.maxLen)
	b.envelope.Segments = append(b.envelope.Segments, Segment{
		Origin: origin, Role: strings.ToLower(strings.TrimSpace(role)), Text: text, Sequence: b.sequence, Trust: SegmentTrustClientSupplied,
	})
	b.sequence++
}

func (b *envelopeBuilder) appendResult(origin SegmentOrigin, role string, result gjson.Result) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	switch {
	case result.IsArray():
		for _, item := range result.Array() {
			b.appendResult(origin, role, item)
		}
	case result.IsObject():
		blockType := strings.ToLower(strings.TrimSpace(result.Get("type").String()))
		switch blockType {
		case "tool_result", "function_call_output", "computer_call_output", "mcp_call_output":
			b.appendResult(OriginToolOutput, "tool", firstExistingResult(result, "output", "content", "text"))
		case "tool_use", "function_call", "computer_call", "mcp_call":
			b.appendToolArguments(result, role)
		default:
			if text := result.Get("text"); text.Type == gjson.String {
				b.append(origin, role, text.String())
			}
			if content := result.Get("content"); content.Exists() {
				b.appendResult(origin, role, content)
			}
		}
		b.appendAttachmentReferences(result, role)
	case result.Type == gjson.String:
		b.append(origin, role, result.String())
	}
}

func (b *envelopeBuilder) appendMessages(result gjson.Result) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	if !result.IsArray() {
		if result.IsObject() {
			if role := strings.ToLower(strings.TrimSpace(result.Get("role").String())); role != "" {
				b.appendMessage(result, role, true)
				return
			}
			if itemType := strings.ToLower(strings.TrimSpace(result.Get("type").String())); itemType != "" {
				b.appendTypedInputItem(result, true)
				return
			}
		}
		b.appendResult(OriginCurrentUser, "user", result)
		return
	}

	items := result.Array()
	lastUser := -1
	previousUser := -1
	hasRoles := false
	for index, item := range items {
		if !item.IsObject() {
			continue
		}
		if roleResult := item.Get("role"); roleResult.Exists() {
			hasRoles = true
			if strings.EqualFold(strings.TrimSpace(roleResult.String()), "user") {
				previousUser = lastUser
				lastUser = index
			}
		}
	}
	segmentStarts := make([]int, len(items))
	segmentEnds := make([]int, len(items))
	for index, item := range items {
		segmentStarts[index] = len(b.envelope.Segments)
		if hasRoles && item.IsObject() && item.Get("role").Exists() {
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			b.appendMessage(item, role, role == "user" && index == lastUser)
		} else {
			b.appendTypedInputItem(item, true)
		}
		segmentEnds[index] = len(b.envelope.Segments)
	}
	if hasRoles && previousUser >= 0 && lastUser >= 0 {
		currentText := make([]string, 0, segmentEnds[lastUser]-segmentStarts[lastUser])
		for _, segment := range b.envelope.Segments[segmentStarts[lastUser]:segmentEnds[lastUser]] {
			if segment.Origin == OriginCurrentUser {
				currentText = append(currentText, segment.Text)
			}
		}
		if isContinuationOnly(strings.Join(currentText, "\n")) {
			for index := segmentStarts[previousUser]; index < segmentEnds[previousUser]; index++ {
				if b.envelope.Segments[index].Origin == OriginHistory {
					b.envelope.Segments[index].Linked = true
				}
			}
		}
	}
}

func (b *envelopeBuilder) appendMessage(message gjson.Result, role string, currentUser bool) {
	origin := OriginHistory
	switch role {
	case "user":
		if currentUser {
			origin = OriginCurrentUser
		}
	case "system":
		origin = OriginSystem
	case "developer":
		origin = OriginDeveloper
	case "tool", "function":
		origin = OriginToolOutput
	}
	if role == "tool" || role == "function" {
		b.appendResult(OriginToolOutput, role, firstExistingResult(message, "content", "output", "text"))
	} else {
		b.appendResult(origin, role, firstExistingResult(message, "content", "text"))
	}
	b.appendToolArguments(message, role)
	b.appendAttachmentReferences(message, role)
}

func (b *envelopeBuilder) appendTypedInputItem(item gjson.Result, currentUser bool) {
	if !item.Exists() || item.Type == gjson.Null {
		return
	}
	if !item.IsObject() {
		b.appendResult(OriginCurrentUser, "user", item)
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
	switch itemType {
	case "function_call_output", "computer_call_output", "mcp_call_output", "tool_result":
		b.appendResult(OriginToolOutput, "tool", firstExistingResult(item, "output", "content", "text"))
	case "function_call", "computer_call", "mcp_call", "tool_use":
		b.appendToolArguments(item, "assistant")
	case "input_file", "input_image", "image_url", "file", "attachment":
		b.appendAttachmentReferences(item, "user")
	default:
		origin := OriginHistory
		if currentUser {
			origin = OriginCurrentUser
		}
		b.appendResult(origin, "user", item)
	}
}

func (b *envelopeBuilder) appendToolArguments(result gjson.Result, role string) {
	if !result.Exists() || !result.IsObject() {
		return
	}
	appendArgument := func(argument gjson.Result) {
		if !argument.Exists() || argument.Type == gjson.Null {
			return
		}
		if argument.Type == gjson.String {
			b.append(OriginToolArguments, role, argument.String())
			return
		}
		b.append(OriginToolArguments, role, argument.Raw)
	}
	appendArgument(result.Get("arguments"))
	appendArgument(result.Get("function.arguments"))
	if strings.EqualFold(strings.TrimSpace(result.Get("type").String()), "tool_use") {
		appendArgument(result.Get("input"))
	}
	if calls := result.Get("tool_calls"); calls.IsArray() {
		for _, call := range calls.Array() {
			appendArgument(call.Get("function.arguments"))
			appendArgument(call.Get("arguments"))
		}
	}
}

func (b *envelopeBuilder) appendAttachmentReferences(result gjson.Result, role string) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	if result.IsArray() {
		for _, item := range result.Array() {
			b.appendAttachmentReferences(item, role)
		}
		return
	}
	if !result.IsObject() {
		return
	}
	result.ForEach(func(key, value gjson.Result) bool {
		switch strings.ToLower(strings.TrimSpace(key.String())) {
		case "file_id", "image_url", "url", "attachment_id", "file":
			if value.Type == gjson.String {
				b.append(OriginAttachmentRefs, role, value.String())
			} else if value.IsObject() {
				b.appendAttachmentReferences(value, role)
			}
		case "attachments", "source":
			b.appendAttachmentReferences(value, role)
		}
		return true
	})
}

func firstExistingResult(result gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		value := result.Get(path)
		if value.Exists() && value.Type != gjson.Null {
			return value
		}
	}
	return gjson.Result{}
}
