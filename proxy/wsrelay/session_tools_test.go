package wsrelay

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"testing"
)

func addSessionWireTools(t *testing.T, body []byte) []byte {
	t.Helper()
	input := gjson.GetBytes(body, "input")
	items := []json.RawMessage{json.RawMessage(`{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"exec_command","description":"run command","parameters":{"type":"object","properties":{"role":{"type":"string"},"file_id":{"type":"string"}}}}]}`)}
	if input.IsArray() {
		for _, item := range input.Array() {
			items = append(items, json.RawMessage(item.Raw))
		}
	} else {
		raw, _ := json.Marshal(map[string]any{"role": "user", "content": input.String()})
		items = append(items, raw)
	}
	raw, _ := json.Marshal(items)
	out, err := sjson.SetRawBytes(body, "input", raw)
	require.NoError(t, err)
	return out
}

func assertSessionWireTools(t *testing.T, body []byte) {
	t.Helper()
	item := gjson.GetBytes(body, `input.#(type=="additional_tools")`)
	require.Equal(t, "developer", item.Get("role").String())
	require.Equal(t, "exec_command", item.Get("tools.0.name").String())
	require.True(t, item.Get("tools.0.parameters.properties.role").Exists())
	require.True(t, item.Get("tools.0.parameters.properties.file_id").Exists())
}
