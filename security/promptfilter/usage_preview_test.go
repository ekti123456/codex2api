package promptfilter

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageUserPreview(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		direct           bool
	}{
		{"unicode", `{"input":"为项目加入可视化UI,是需要每个工程都加吗"}`, "为项目加入可视化UI,是需要每个工程都加…", true},
		{"whitespace", `{"input":[{"role":"user","content":"  继续\n 执行\t测试 "}]}`, "继续 执行 测试", true},
		{"current only", `{"input":[{"role":"user","content":"旧问题"},{"role":"assistant","content":"答复"},{"role":"user","content":[{"type":"input_text","text":"新问题"}]}]}`, "新问题", true},
		{"tool continuation", `{"input":[{"role":"user","content":"旧问题"},{"type":"function_call_output","output":"工具结果"}]}`, "", false},
		{"assistant replay", `{"input":[{"role":"user","content":"旧问题"},{"role":"assistant","content":"完成"}]}`, "", false},
		{"system only", `{"input":[{"role":"developer","content":"指令"}]}`, "", false},
		{"wrapper", `{"input":"# Files mentioned by the user:\nfile.txt\n\n## My request:\n请检查接口"}`, "请检查接口", true},
		{"environment", `{"input":"<environment_context>cwd</environment_context>"}`, "", false},
		{"compaction", `{"input":"You are performing a CONTEXT CHECKPOINT COMPACTION. Create a summary."}`, "", false},
		{"image", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"secret"}]}]}`, "图片或附件消息", true},
		{"text and image", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"secret"},{"type":"input_text","text":"解释这张图"}]}]}`, "解释这张图", true},
		{"adjacent blocks", `{"input":[{"type":"input_text","text":"检查"},{"type":"input_text","text":"接口"}]}`, "检查 接口", true},
		{"emoji", `{"input":"😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀"}`, "😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀…", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, direct := UsageUserPreview([]byte(tc.body))
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.direct, direct)
		})
	}
}
