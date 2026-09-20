package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSseDataLinesLargeInput 守护 sseDataLines 的关键不变量：返回的每个切片必须
// 拥有独立内存，不能悬挂于 bufio.Scanner 内部 buffer。输入超过 scanner 默认 64KB
// token buffer 时会多次 Fill，若切片指向 buffer，旧字节被新数据覆盖，调用方拿到损坏
// 内容（流式 assistant 解析出乱码/碎片、usage 解析失败；短响应因未跨 Fill 而侥幸正常，
// 长响应 >64KB 才暴露）。构造 >64KB 的 SSE 流，断言每行都能独立 Unmarshal 且内容、
// 顺序与写入一致。
func TestSseDataLinesLargeInput(t *testing.T) {
	var buf bytes.Buffer
	const n = 2000 // 每行约 40B，2000 行 ≈ 80KB，超过 scanner 默认 64KB buffer
	for i := 0; i < n; i++ {
		buf.WriteString("data: ")
		buf.WriteString(fmt.Sprintf(`{"i":%d,"content":"第%d段数据"}`, i, i))
		buf.WriteString("\n\n")
	}
	require.Greater(t, buf.Len(), 64*1024, "测试输入必须超过 scanner 默认 64KB buffer")

	lines := sseDataLines(buf.Bytes())
	require.Len(t, lines, n, "应返回全部 data 行")

	for i, line := range lines {
		var got struct {
			I       int    `json:"i"`
			Content string `json:"content"`
		}
		require.NoErrorf(t, json.Unmarshal(line, &got), "第 %d 行必须可独立解析（切片不能指向 scanner buffer）", i)
		require.Equalf(t, i, got.I, "第 %d 行内容必须与写入顺序一致（buffer 覆盖会错乱）", i)
	}
}

// TestSseDataLinesSkipsNonData 验证拆行语义：跳过 event: 行、空行、注释行与 [DONE]，
// 剥掉 data: 前缀，仅保留 JSON 数据载荷，并去除首尾空白。
func TestSseDataLinesSkipsNonData(t *testing.T) {
	in := []byte("event: ping\n\ndata: {\"a\":1}\n\nevent: pong\n\ndata: [DONE]\n\n: comment\n\ndata:   {\"a\":2}  \n\n")
	lines := sseDataLines(in)
	require.Equal(t, [][]byte{[]byte(`{"a":1}`), []byte(`{"a":2}`)}, lines)
}
