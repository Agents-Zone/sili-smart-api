package service

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestParseRequestMessages 三协议请求 messages 归一化：OpenAI role=tool、
// Claude tool_result block、Gemini contents[]，全部按请求原序输出。
func TestParseRequestMessages(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		want []MsgPart
	}{
		{
			name: "openai_chat_with_tool_role",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-4o","messages":[
				{"role":"system","content":"You are helpful"},
				{"role":"user","content":"北京天气怎么样？"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"晴，25度"}
			]}`,
			want: []MsgPart{
				{Role: msgRoleSystem, Kind: msgKindText, Text: "You are helpful"},
				{Role: msgRoleUser, Kind: msgKindText, Text: "北京天气怎么样？"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
				{Role: msgRoleTool, Kind: msgKindToolResult, Text: "晴，25度"},
			},
		},
		{
			name: "openai_assistant_content_and_tool_calls",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-4o","messages":[
				{"role":"assistant","content":"让我查一下","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]}
			]}`,
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "让我查一下"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
		{
			name: "openai_completions_no_messages",
			path: "/v1/completions",
			body: `{"model":"gpt-3.5-turbo","prompt":"hello"}`,
			want: []MsgPart{},
		},
		{
			name: "openai_responses_parses_input",
			path: "/v1/responses",
			body: `{"model":"gpt-5","input":[
				{"role":"user","content":[{"type":"input_text","text":"北京天气怎么样？"}]},
				{"role":"tool","call_id":"call_1","content":[{"type":"output_text","text":"晴，25度"}]}
			]}`,
			want: []MsgPart{
				{Role: msgRoleUser, Kind: msgKindText, Text: "北京天气怎么样？"},
				{Role: msgRoleTool, Kind: msgKindToolResult, Text: "晴，25度"},
			},
		},
		{
			name: "openai_responses_string_input",
			path: "/v1/responses/compact",
			body: `{"model":"gpt-5","input":"hello"}`,
			want: []MsgPart{
				{Role: msgRoleUser, Kind: msgKindText, Text: "hello"},
			},
		},
		{
			name: "claude_tool_result_block",
			path: "/v1/messages",
			body: `{"model":"claude-3-5-sonnet","messages":[
				{"role":"user","content":[
					{"type":"text","text":"请查天气"},
					{"type":"tool_result","tool_use_id":"toolu_1","content":"晴，25度"}
				]}
			]}`,
			want: []MsgPart{
				{Role: msgRoleUser, Kind: msgKindText, Text: "请查天气"},
				{Role: msgRoleTool, Kind: msgKindToolResult, Text: "晴，25度"},
			},
		},
		{
			name: "claude_string_content",
			path: "/v1/messages",
			body: `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"你好"}]}`,
			want: []MsgPart{
				{Role: msgRoleUser, Kind: msgKindText, Text: "你好"},
			},
		},
		{
			name: "claude_assistant_tool_use_in_history",
			path: "/v1/messages",
			body: `{"model":"claude-3-5-sonnet","messages":[
				{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}]}
			]}`,
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
		{
			name: "gemini_contents_text_and_function_call",
			path: "/v1beta/models/gemini-1.5-pro:generateContent",
			body: `{"contents":[
				{"role":"user","parts":[{"text":"北京天气？"}]},
				{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"北京"}}}]}
			]}`,
			want: []MsgPart{
				{Role: msgRoleUser, Kind: msgKindText, Text: "北京天气？"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
		{
			name: "gemini_function_response",
			path: "/v1/models/gemini:streamGenerateContent",
			body: `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"result":"晴"}}}]}]}`,
			want: []MsgPart{
				{Role: msgRoleTool, Kind: msgKindToolResult, Text: `{"result":"晴"}`},
			},
		},
		{
			name: "unknown_path_empty",
			path: "/v1/embeddings",
			body: `{"input":"x"}`,
			want: []MsgPart{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRequestMessages(tc.path, []byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseRequestMessagesToolResult 核心断言：OpenAI role=tool 与 Claude
// tool_result block 均标 Kind=tool_result 且 Role=tool；OpenAI Responses 的 input[]
// 元素 role=tool 同样标 Kind=tool_result。
func TestParseRequestMessagesToolResult(t *testing.T) {
	openAI, err := parseRequestMessages("/v1/chat/completions", []byte(`{"messages":[{"role":"tool","content":"晴"}]}`))
	require.NoError(t, err)
	require.Len(t, openAI, 1)
	assert.Equal(t, msgRoleTool, openAI[0].Role)
	assert.Equal(t, msgKindToolResult, openAI[0].Kind)

	claude, err := parseRequestMessages("/v1/messages", []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"晴"}]}]}`))
	require.NoError(t, err)
	require.Len(t, claude, 1)
	assert.Equal(t, msgRoleTool, claude[0].Role)
	assert.Equal(t, msgKindToolResult, claude[0].Kind)

	responses, err := parseRequestMessages("/v1/responses", []byte(`{"input":[{"role":"user","content":"问"},{"role":"tool","content":"晴"}]}`))
	require.NoError(t, err)
	require.Len(t, responses, 2)
	assert.Equal(t, msgRoleUser, responses[0].Role)
	assert.Equal(t, msgRoleTool, responses[1].Role)
	assert.Equal(t, msgKindToolResult, responses[1].Kind)
}

// TestParseRequestMessagesError 非法 JSON 返回 error；空 body 返回 error。
func TestParseRequestMessagesError(t *testing.T) {
	_, err := parseRequestMessages("/v1/chat/completions", []byte(`{"messages":[`))
	assert.Error(t, err)

	_, err = parseRequestMessages("/v1/messages", []byte(""))
	assert.Error(t, err)

	_, err = parseRequestMessages("/v1beta/models/gemini-1.5-pro:generateContent", []byte(`not json`))
	assert.Error(t, err)
}

// TestParseAssistantContentNonStream 非流式解析：OpenAI chat/completions 的
// message.content 与 message.tool_calls、OpenAI completions 的 choices[].text、
// OpenAI Responses 的 output[]、Claude content[]、Gemini candidates[].content.parts[]。
func TestParseAssistantContentNonStream(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		want []MsgPart
	}{
		{
			name: "openai_chat_text_and_tool_calls",
			path: "/v1/chat/completions",
			body: `{"choices":[{"index":0,"message":{"role":"assistant","content":"让我查一下","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]}}]}`,
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "让我查一下"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
		{
			name: "openai_completions_legacy_text",
			path: "/v1/completions",
			body: `{"choices":[{"index":0,"text":"Hello, world"}]}`,
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "Hello, world"},
			},
		},
		{
			name: "openai_responses_message_and_function_call",
			path: "/v1/responses",
			body: `{"output":[
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"答案"}]},
				{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"}
			]}`,
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "答案"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
		{
			name: "claude_text_and_tool_use",
			path: "/v1/messages",
			body: `{"content":[{"type":"text","text":"好的"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}]}`,
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "好的"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
		{
			name: "gemini_text_and_function_call",
			path: "/v1beta/models/gemini-1.5-pro:generateContent",
			body: `{"candidates":[{"content":{"role":"model","parts":[{"text":"天气"},{"functionCall":{"name":"get_weather","args":{"city":"北京"}}}]}}]}`,
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "天气"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAssistantContent(tc.path, false, []byte(tc.body))
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseAssistantContentStream 流式 SSE 解析：逐行扫描、剥 data: 前缀、
// 跳 event:/[DONE]，按协议分派增量位置并正确累积。
func TestParseAssistantContentStream(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		want []MsgPart
	}{
		{
			name: "openai_chat_text_accumulate",
			path: "/v1/chat/completions",
			body: "event: message\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"}}]}\n\ndata: [DONE]\n",
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "你好，世界"},
			},
		},
		{
			name: "openai_chat_reasoning_and_content",
			path: "/v1/chat/completions",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"思考\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"回答\"}}]}\n\ndata: [DONE]\n",
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "思考回答"},
			},
		},
		{
			name: "openai_chat_tool_calls_accumulate",
			path: "/v1/chat/completions",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"}}]}}]}\n\ndata: [DONE]\n",
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
		{
			name: "openai_chat_tool_calls_name_missing_skipped",
			path: "/v1/chat/completions",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"\",\"arguments\":\"\"}}]}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"}}]}}]}\n\ndata: [DONE]\n",
			want: []MsgPart{},
		},
		{
			name: "openai_responses_stream",
			path: "/v1/responses",
			body: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"答案\"}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"name\":\"get_weather\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"delta\":\"{\\\"city\\\":\\\"北京\\\"}\"}\n\ndata: [DONE]\n",
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "答案"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
		{
			name: "claude_stream_text_and_tool",
			path: "/v1/messages",
			body: "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"name\":\"get_weather\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"北京\\\"}\"}}\n\ndata: [DONE]\n",
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "你好"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
		{
			name: "gemini_stream_text_and_function_call",
			path: "/v1/models/gemini:streamGenerateContent",
			body: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"天气\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"北京\"}}}]}}]}\n\ndata: [DONE]\n",
			want: []MsgPart{
				{Role: msgRoleAssistant, Kind: msgKindText, Text: "天气"},
				{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAssistantContent(tc.path, true, []byte(tc.body))
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseAssistantContentEmptyOrError 错误响应/空响应/未知路径记空 []MsgPart。
func TestParseAssistantContentEmptyOrError(t *testing.T) {
	// 上游 4xx/5xx 错误 JSON，无消息体
	got := parseAssistantContent("/v1/chat/completions", false, []byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`))
	assert.Empty(t, got)

	got = parseAssistantContent("/v1/messages", false, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
	assert.Empty(t, got)

	// 空响应体
	got = parseAssistantContent("/v1/chat/completions", false, []byte(""))
	assert.Empty(t, got)

	// 流式空响应
	got = parseAssistantContent("/v1/chat/completions", true, []byte(""))
	assert.Empty(t, got)

	// 未知路径
	got = parseAssistantContent("/v1/embeddings", false, []byte(`{"data":[]}`))
	assert.Empty(t, got)
}

// TestParseUsage 三协议 × 流式/非流式的 token 解析。
func TestParseUsage(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		isStream       bool
		body           string
		wantPrompt     int
		wantCompletion int
	}{
		{
			name:           "openai_non_stream_top_usage",
			path:           "/v1/chat/completions",
			body:           `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			wantPrompt:     10,
			wantCompletion: 5,
		},
		{
			name:           "openai_stream_last_chunk_no_usage",
			path:           "/v1/chat/completions",
			isStream:       true,
			body:           "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n",
			wantPrompt:     0,
			wantCompletion: 0,
		},
		{
			name:           "openai_stream_usage_in_last_chunk",
			path:           "/v1/chat/completions",
			isStream:       true,
			body:           "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n",
			wantPrompt:     10,
			wantCompletion: 5,
		},
		{
			name:           "openai_responses_non_stream_input_output",
			path:           "/v1/responses",
			body:           `{"output":[{"type":"message","content":[{"type":"output_text","text":"x"}]}],"usage":{"input_tokens":12,"output_tokens":8}}`,
			wantPrompt:     12,
			wantCompletion: 8,
		},
		{
			name:           "openai_responses_stream_completed_event",
			path:           "/v1/responses",
			isStream:       true,
			body:           "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":15,\"output_tokens\":6}}}\n\ndata: [DONE]\n",
			wantPrompt:     15,
			wantCompletion: 6,
		},
		{
			name:           "claude_non_stream_top_usage",
			path:           "/v1/messages",
			body:           `{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":8,"output_tokens":4}}`,
			wantPrompt:     8,
			wantCompletion: 4,
		},
		{
			name:           "claude_stream_merge_message_start_and_delta",
			path:           "/v1/messages",
			isStream:       true,
			body:           "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":0}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\ndata: [DONE]\n",
			wantPrompt:     20,
			wantCompletion: 9,
		},
		{
			name:           "gemini_non_stream_usage_metadata",
			path:           "/v1beta/models/gemini-1.5-pro:generateContent",
			body:           `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":7}}`,
			wantPrompt:     30,
			wantCompletion: 7,
		},
		{
			name:           "gemini_stream_last_chunk_usage_metadata",
			path:           "/v1/models/gemini:streamGenerateContent",
			isStream:       true,
			body:           "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\ndata: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":30,\"candidatesTokenCount\":7}}\n\ndata: [DONE]\n",
			wantPrompt:     30,
			wantCompletion: 7,
		},
		{
			name:           "malformed_body_zero",
			path:           "/v1/chat/completions",
			body:           `not json`,
			wantPrompt:     0,
			wantCompletion: 0,
		},
		{
			name:           "unknown_path_zero",
			path:           "/v1/embeddings",
			body:           `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			wantPrompt:     0,
			wantCompletion: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPrompt, gotCompletion := parseUsage(tc.path, tc.isStream, []byte(tc.body))
			assert.Equal(t, tc.wantPrompt, gotPrompt)
			assert.Equal(t, tc.wantCompletion, gotCompletion)
		})
	}
}

// TestMsgPartRoundTrip MsgPart 经 common.Marshal/common.Unmarshal 往返一致（JSON 合规）。
func TestMsgPartRoundTrip(t *testing.T) {
	parts := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "你好，世界"},
		{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
		{Role: msgRoleTool, Kind: msgKindToolResult, Text: "晴，25度"},
		{Role: msgRoleSystem, Kind: msgKindText, Text: "system prompt"},
	}
	data, err := common.Marshal(parts)
	require.NoError(t, err)

	var decoded []MsgPart
	require.NoError(t, common.Unmarshal(data, &decoded))
	assert.Equal(t, parts, decoded)
}

// TestProtocolForPath 路径白名单到协议常量的映射，T3/T4 复用同一判定。
func TestProtocolForPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", openAIProtocol},
		{"/v1/completions", openAIProtocol},
		{"/v1/responses", openAIProtocol},
		{"/v1/responses/compact", openAIProtocol},
		{"/v1/messages", claudeProtocol},
		{"/v1beta/models/gemini-1.5-pro:generateContent", geminiProtocol},
		{"/v1/models/gemini:streamGenerateContent", geminiProtocol},
		{"/v1/embeddings", ""},
		{"/v1/audio/transcriptions", ""},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, protocolForPath(tc.path))
		})
	}
}

// TestFingerprintMessages 核心断言：同 parts 同指纹；不同 parts 不同指纹；全字段不截断
// （64 字符之后的差异仍能区分，旧 prefixHash 截断会误判相同）；role 参与 fingerprint；
// 空输入确定。
func TestFingerprintMessages(t *testing.T) {
	parts := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "你好"}}
	same := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "你好"}}
	different := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "今天天气怎么样"}}

	h1 := fingerprintMessages(parts)
	h2 := fingerprintMessages(same)
	h3 := fingerprintMessages(different)
	assert.NotEmpty(t, h1)
	assert.Equal(t, h1, h2, "同内容必须得到相同指纹")
	assert.NotEqual(t, h1, h3, "不同内容必须得到不同指纹")

	// 不截断：前 64 字符相同、之后不同的长文本，指纹不同。
	longX := strings.Repeat("a", 80) + "X"
	longY := strings.Repeat("a", 80) + "Y"
	assert.NotEqual(t,
		fingerprintMessages([]MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: longX}}),
		fingerprintMessages([]MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: longY}}),
		"全字段指纹不截断，64 字符之后的差异必须区分")

	// role 参与 fingerprint：同 text 不同 role 指纹不同。
	assert.NotEqual(t,
		fingerprintMessages([]MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "t"}}),
		fingerprintMessages([]MsgPart{{Role: msgRoleAssistant, Kind: msgKindText, Text: "t"}}),
		"role 参与 fingerprint")

	// 空输入确定指纹。
	assert.NotEmpty(t, fingerprintMessages(nil))
	assert.Equal(t, fingerprintMessages(nil), fingerprintMessages([]MsgPart{}), "空切片与 nil 指纹一致")
}

// useIndependentSessionRedis 为会话缓存测试搭一个独立 miniredis，保存并恢复
// common.RedisEnabled 与 common.RDB。
func useIndependentSessionRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	previousRedisEnabled := common.RedisEnabled
	previousRDB := common.RDB
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	common.RedisEnabled = true
	common.RDB = client
	t.Cleanup(func() {
		_ = client.Close()
		common.RedisEnabled = previousRedisEnabled
		common.RDB = previousRDB
	})
	return server
}

// TestResolveSessionKeySingleInstanceDegraded 多槽续链语义（退化模式）：条数严格增长的
// requestParts 序列（无状态客户端历史叠加）续链同一 sessionKey；条数不增长的同内容重复
// 请求判为新会话；不同 tokenID 分桶；同 token 的非前缀包含会话分属不同 slot。
func TestResolveSessionKeySingleInstanceDegraded(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	sessionKeyLocalMap = sync.Map{}
	singleInstanceLogOnce = sync.Once{}
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	// 多轮：条数严格增长，前缀完整包含上一轮。
	turn1 := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "你好"}}
	turn2 := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "你好"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "你好啊"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "今天天气怎么样"},
	}
	turn3 := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "你好"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "你好啊"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "今天天气怎么样"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "晴天"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "谢谢"},
	}

	sk1, isNew1 := resolveSessionKey(1, turn1)
	sk2, isNew2 := resolveSessionKey(1, turn2)
	sk3, isNew3 := resolveSessionKey(1, turn3)
	assert.NotEmpty(t, sk1)
	assert.True(t, isNew1, "首轮新建")
	assert.Equal(t, sk1, sk2, "续链必须复用同一 sessionKey")
	assert.False(t, isNew2, "续链非新建")
	assert.Equal(t, sk1, sk3, "多轮续链同一 sessionKey")
	assert.False(t, isNew3)

	// 条数不增长的同内容重复请求判为新会话（与旧 prefixHash 内容指纹语义不同）。
	skRepeat, isNewRepeat := resolveSessionKey(1, turn1)
	assert.True(t, isNewRepeat, "条数不增长的同内容请求判为新会话")
	assert.NotEqual(t, sk1, skRepeat)

	// 不同 tokenID 分桶。
	skOther, _ := resolveSessionKey(2, turn1)
	assert.NotEqual(t, sk1, skOther, "不同 tokenID 分桶")

	// 同 token 的非前缀包含会话分属不同 slot。
	diffConv := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "完全不同的开场"}}
	skDiff, isNewDiff := resolveSessionKey(1, diffConv)
	assert.True(t, isNewDiff)
	assert.NotEqual(t, sk1, skDiff, "不同会话分属不同 slot")
}

// TestResolveSessionKeyNilParts 空 requestParts 边界：nil 不 panic，sessionKey 非空；
// 空 → 非空的条数增长续链（空首轮 Count=0，下一轮非空 thisCount>0 且前 0 条指纹相等）。
// 覆盖退化单实例与 Redis 可用两条路径。
func TestResolveSessionKeyNilParts(t *testing.T) {
	t.Run("degraded_single_instance", func(t *testing.T) {
		previousRedisEnabled := common.RedisEnabled
		common.RedisEnabled = false
		sessionKeyLocalMap = sync.Map{}
		singleInstanceLogOnce = sync.Once{}
		t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

		sk1, isNew1 := resolveSessionKey(1, nil)
		assert.NotEmpty(t, sk1, "空输入必须得到非空 sessionKey")
		assert.True(t, isNew1, "空首轮新建")

		// 条数增长续链：空 → 非空。
		next := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "续"}}
		sk2, isNew2 := resolveSessionKey(1, next)
		assert.Equal(t, sk1, sk2, "空首轮后非空续轮续链同一 sessionKey")
		assert.False(t, isNew2)
	})

	t.Run("redis_available", func(t *testing.T) {
		server := useIndependentSessionRedis(t)

		sk1, isNew1 := resolveSessionKey(1, nil)
		assert.NotEmpty(t, sk1)
		assert.True(t, isNew1)

		next := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "续"}}
		sk2, isNew2 := resolveSessionKey(1, next)
		assert.Equal(t, sk1, sk2, "Redis 路径空首轮后非空续轮续链")
		assert.False(t, isNew2)

		// 多槽 JSON 落盘：key 为 conv:session:{token_id}（无 prefixHash 后缀）。
		key := convSessionCachePrefix + "1"
		val, err := server.Get(key)
		require.NoError(t, err)
		assert.Contains(t, val, sk1, "多槽 JSON 必须落盘且含本轮 sessionKey")
	})
}

// TestResolveSessionKeyDegradedLogsSingleInstance Redis 未配置时首例退化必须经
// common.SysError 记录单实例模式（sync.Once 保护，只标注一次），即 BR4。日志断言只
// 绑定 SysError 输出通道的固定 [SYS] 前缀，不绑定具体消息文案，文案调整不会脆化。
func TestResolveSessionKeyDegradedLogsSingleInstance(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	singleInstanceLogOnce = sync.Once{}
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	var logBuffer bytes.Buffer
	common.LogWriterMu.Lock()
	previousErrorWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousErrorWriter
		common.LogWriterMu.Unlock()
	})

	sk, isNew := resolveSessionKey(7, []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "退化标注"}})
	assert.NotEmpty(t, sk, "退化模式必须返回确定 sessionKey")
	assert.True(t, isNew, "退化模式首次调用必须判定为新会话")
	assert.Contains(t, logBuffer.String(), "[SYS]", "退化模式必须触发 SysError 错误记录")

	// sync.Once 保证只标注一次：再次调用不再新增日志
	logBuffer.Reset()
	sk2, _ := resolveSessionKey(8, []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "再次调用"}})
	assert.Empty(t, logBuffer.String(), "单实例标注必须只输出一次")
	assert.NotEmpty(t, sk2, "再次调用仍须返回确定 sessionKey")
}

// TestResolveSessionKeyRedisHitMissRenew Redis 可用：首轮新建落盘多槽 JSON，续链
// （条数增长 + 前缀包含）命中同一 sessionKey 并滚动续期 TTL；缓存键形如
// conv:session:{token_id}（多槽 JSON 数组），TTL 30min。
func TestResolveSessionKeyRedisHitMissRenew(t *testing.T) {
	server := useIndependentSessionRedis(t)
	turn1 := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "你好"}}
	turn2 := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "你好"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "你好啊"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "第二问"},
	}

	sk1, isNew1 := resolveSessionKey(1, turn1)
	assert.True(t, isNew1)
	assert.NotEmpty(t, sk1)

	key := convSessionCachePrefix + "1"
	val, err := server.Get(key)
	require.NoError(t, err)
	assert.Contains(t, val, sk1, "多槽 JSON 必须缓存本轮 sessionKey")
	ttl := server.TTL(key)
	assert.True(t, ttl > 0 && ttl <= convSessionCacheTTL, "缓存 TTL 应为 30min，实际 %v", ttl)

	// 续链命中同一 sessionKey。
	sk2, isNew2 := resolveSessionKey(1, turn2)
	assert.Equal(t, sk1, sk2, "续链必须复用 sessionKey")
	assert.False(t, isNew2)

	server.FastForward(29 * time.Minute)
	turn3 := append(turn2, MsgPart{Role: msgRoleUser, Kind: msgKindText, Text: "第三问"})
	_, isNew3 := resolveSessionKey(1, turn3)
	assert.False(t, isNew3, "TTL 内滚动续期后仍应命中")
	assert.True(t, server.TTL(key) > 29*time.Minute, "续链写回后 TTL 应被滚动续期")
}

// TestResolveSessionKeySkillCallIsolatedSlot 同 token 的技能/标题生成等子请求与主对话
// 分属不同 slot：主对话续链不被技能调用（不同前缀）污染，各自独立 sessionKey。
func TestResolveSessionKeySkillCallIsolatedSlot(t *testing.T) {
	useIndependentSessionRedis(t)

	mainTurn1 := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "主对话第一问"}}
	mainTurn2 := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "主对话第一问"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "回答"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "主对话第二问"},
	}
	// 技能调用：完全不同的开场（如生成对话标题）。
	skillTurn := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "请生成对话标题"}}

	mainSk1, _ := resolveSessionKey(1, mainTurn1)
	skillSk, skillIsNew := resolveSessionKey(1, skillTurn)
	assert.NotEqual(t, mainSk1, skillSk, "技能调用与主对话分属不同 slot")
	assert.True(t, skillIsNew)

	// 技能调用不污染主对话续链：主对话第二轮仍续到主对话 sessionKey。
	mainSk2, mainIsNew2 := resolveSessionKey(1, mainTurn2)
	assert.Equal(t, mainSk1, mainSk2, "技能调用插入后主对话仍续链同一 sessionKey")
	assert.False(t, mainIsNew2)
}

// TestResolveSessionKeyRedisRuntimeErrorFallback Redis 运行期故障（连接失败）时
// 捕获链路不中断，按新会话回退生成 sessionKey 并 SysError 记录（BR5）。日志断言只
// 绑定 SysError 输出通道的固定 [SYS] 前缀，不绑定具体消息文案。
func TestResolveSessionKeyRedisRuntimeErrorFallback(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	previousRDB := common.RDB
	badClient := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		MaxRetries:  0,
		DialTimeout: 300 * time.Millisecond,
	})
	common.RedisEnabled = true
	common.RDB = badClient
	t.Cleanup(func() {
		_ = badClient.Close()
		common.RedisEnabled = previousRedisEnabled
		common.RDB = previousRDB
	})

	var logBuffer bytes.Buffer
	common.LogWriterMu.Lock()
	previousErrorWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousErrorWriter
		common.LogWriterMu.Unlock()
	})

	parts := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "故障回退"}}
	sk, isNew := resolveSessionKey(4, parts)
	assert.NotEmpty(t, sk)
	assert.True(t, isNew, "Redis 运行期故障必须按新会话回退")
	assert.Contains(t, logBuffer.String(), "[SYS]", "运行期故障必须触发 SysError 错误记录")
}

// TestMatchSessionSlotsTimeoutEvict 超时 slot 被 prune：ActiveTime 早于
//（now - convSessionSlotTimeout）的 slot 不参与续链判定，等价于新会话；TTL 内续链命中。
func TestMatchSessionSlotsTimeoutEvict(t *testing.T) {
	now := int64(1000000)
	turn1 := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "x"}}
	sk, isNew, slots := matchSessionSlots(nil, turn1, now)
	require.True(t, isNew)
	require.NotEmpty(t, sk)
	require.Len(t, slots, 1)

	turn2 := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "x"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "a"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "y"},
	}

	// 30min+1s 后，旧 slot 超时；同前缀的续轮判为新会话（旧 slot 被 prune）。
	later := now + int64((convSessionSlotTimeout+time.Second)/time.Second)
	sk2, isNew2, slots2 := matchSessionSlots(slots, turn2, later)
	require.True(t, isNew2, "旧 slot 超时淘汰，续轮判为新会话")
	require.NotEqual(t, sk, sk2)
	require.Len(t, slots2, 1, "超时 slot 被 prune，仅新 slot")

	// TTL 内（30s 前）续链命中。
	soon := now + int64((convSessionSlotTimeout-time.Second)/time.Second)
	sk3, isNew3, _ := matchSessionSlots(slots, turn2, soon)
	require.False(t, isNew3, "TTL 内续链命中")
	assert.Equal(t, sk, sk3)
}

// TestMatchSessionSlotsCountMustGrow 条数严格增长检查：字节级相同的请求体重放（条数
// 不增长）判为新会话，零成本打散顺序重放误并；条数增长且前缀包含才续链。
func TestMatchSessionSlotsCountMustGrow(t *testing.T) {
	now := int64(1000000)
	turn := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "same"}}

	sk1, _, slots := matchSessionSlots(nil, turn, now)
	// 同内容同条数重放：条数不增长，判为新会话。
	sk2, isNew2, slots := matchSessionSlots(slots, turn, now)
	assert.True(t, isNew2, "条数不增长判为新会话")
	assert.NotEqual(t, sk1, sk2)
	require.Len(t, slots, 2, "两次各占一个 slot")

	// 条数增长且前缀包含才续链，命中最先建立的 slot。
	turnGrow := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "same"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "a"},
	}
	sk3, isNew3, _ := matchSessionSlots(slots, turnGrow, now)
	assert.False(t, isNew3, "条数增长 + 前缀包含续链")
	assert.Equal(t, sk1, sk3)
}

// TestJoinConversationPartsIsNewBranch isNew 分支与切点 off-by-one：isNew=true 存全量；
// isNew=false 切点为 lastAssistantIndex+1（跳过上一轮 assistant，避免拼接重复）；续链
// 找不到 assistant 回退全量。
func TestJoinConversationPartsIsNewBranch(t *testing.T) {
	req := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "u1"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "a1"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "u2"},
	}
	asst := []MsgPart{{Role: msgRoleAssistant, Kind: msgKindText, Text: "a2"}}

	// isNew=true 全量。
	got := joinConversationParts(req, asst, true)
	require.Len(t, got, 4)
	assert.Equal(t, "u1", got[0].Text)
	assert.Equal(t, "a2", got[3].Text)

	// isNew=false 切点 off-by-one：切点在 a1 之后，increment=[u2]，+[a2]。
	got = joinConversationParts(req, asst, false)
	require.Len(t, got, 2, "增量仅 u2 + a2，历史 u1/a1 不重复")
	assert.Equal(t, "u2", got[0].Text)
	assert.Equal(t, "a2", got[1].Text)

	// 续链找不到 assistant（理论不发生）回退全量。
	noAsst := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "only-user"}}
	got = joinConversationParts(noAsst, asst, false)
	require.Len(t, got, 2, "找不到 assistant 回退全量")
	assert.Equal(t, "only-user", got[0].Text)
}

// TestLastAssistantIndex 找最后一个 role=assistant 的位置；tool_use（Role=assistant）
// 同样命中；无 assistant 返回 -1。
func TestLastAssistantIndex(t *testing.T) {
	assert.Equal(t, -1, lastAssistantIndex(nil))
	assert.Equal(t, -1, lastAssistantIndex([]MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "u"}}))
	assert.Equal(t, 1, lastAssistantIndex([]MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "u"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "a"},
	}))
	assert.Equal(t, 2, lastAssistantIndex([]MsgPart{
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "a1"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "u"},
		{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: "tool()"},
	}), "tool_use（Role=assistant）同样命中且取最后一个")
}

// TestResolveSessionKeyLocalMultiSlotIsolation 退化模式（Mutex 保护）下，同 token 的
// 多个不同会话（非前缀包含）各自独立 slot，连续解析不互相覆盖、不丢失。
func TestResolveSessionKeyLocalMultiSlotIsolation(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	sessionKeyLocalMap = sync.Map{}
	singleInstanceLogOnce = sync.Once{}
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	convA := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "A1"}}
	convB := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "B1"}}

	skA1, _ := resolveSessionKey(1, convA)
	skB1, _ := resolveSessionKey(1, convB)
	assert.NotEqual(t, skA1, skB1, "不同会话不同 slot")

	convA2 := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "A1"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "a"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "A2"},
	}
	skA2, isNewA2 := resolveSessionKey(1, convA2)
	assert.Equal(t, skA1, skA2, "A 续链不被 B 干扰")
	assert.False(t, isNewA2)

	convB2 := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "B1"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "b"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "B2"},
	}
	skB2, isNewB2 := resolveSessionKey(1, convB2)
	assert.Equal(t, skB1, skB2, "B 续链不被 A 干扰")
	assert.False(t, isNewB2)
}

// TestTurnKindFor 核心断言（BR1）：含 tool_use/tool_result 记 tool_round（优先级最高），
// 无工具且 isNew=true 记 first，无工具且 isNew=false 记 normal。
func TestTurnKindFor(t *testing.T) {
	t.Run("tool_use_wins_over_first_and_normal", func(t *testing.T) {
		toolParts := []MsgPart{{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`}}
		assert.Equal(t, turnKindToolRound, turnKindFor(toolParts, true), "含 tool_use 且新会话记 tool_round（工具优先）")
		assert.Equal(t, turnKindToolRound, turnKindFor(toolParts, false), "含 tool_use 且非新会话记 tool_round（工具优先）")
	})

	t.Run("tool_result_also_tool_round", func(t *testing.T) {
		resultParts := []MsgPart{{Role: msgRoleTool, Kind: msgKindToolResult, Text: "晴，25度"}}
		assert.Equal(t, turnKindToolRound, turnKindFor(resultParts, true), "含 tool_result 且新会话记 tool_round")
		assert.Equal(t, turnKindToolRound, turnKindFor(resultParts, false), "含 tool_result 且非新会话记 tool_round")
	})

	t.Run("mixed_text_and_tool", func(t *testing.T) {
		mixed := []MsgPart{
			{Role: msgRoleUser, Kind: msgKindText, Text: "查天气"},
			{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: "get_weather()"},
		}
		assert.Equal(t, turnKindToolRound, turnKindFor(mixed, true), "文本与工具混合仍记 tool_round")
	})

	t.Run("no_tool_first_or_normal", func(t *testing.T) {
		pureText := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "你好"}}
		assert.Equal(t, turnKindFirst, turnKindFor(pureText, true), "无工具且新会话记 first")
		assert.Equal(t, turnKindNormal, turnKindFor(pureText, false), "无工具且非新会话记 normal")
	})

	t.Run("empty_parts", func(t *testing.T) {
		assert.Equal(t, turnKindFirst, turnKindFor(nil, true), "空 parts 且新会话记 first")
		assert.Equal(t, turnKindNormal, turnKindFor(nil, false), "空 parts 且非新会话记 normal")
		assert.Equal(t, turnKindFirst, turnKindFor([]MsgPart{}, true), "空切片且新会话记 first")
		assert.Equal(t, turnKindNormal, turnKindFor([]MsgPart{}, false), "空切片且非新会话记 normal")
	})
}

// TestMergeConversation 核心断言（BR4）：按传入顺序逐行 append messages 列得到
// 完整 []MsgPart 序列，元素带 role/kind。
func TestMergeConversation(t *testing.T) {
	turns := []model.ConversationTurn{
		{Messages: `[{"role":"user","kind":"text","text":"a"}]`},
		{Messages: `[{"role":"assistant","kind":"text","text":"b"}]`},
	}
	got := MergeConversation(turns)
	require.Len(t, got, 2, "两行消息按序拼接应为 2 段")
	assert.Equal(t, msgRoleUser, got[0].Role)
	assert.Equal(t, msgKindText, got[0].Kind)
	assert.Equal(t, "a", got[0].Text)
	assert.Equal(t, msgRoleAssistant, got[1].Role)
	assert.Equal(t, msgKindText, got[1].Kind)
	assert.Equal(t, "b", got[1].Text)
}

// TestMergeConversationInvalidRowSkipped 核心断言：非法 JSON 行记空并 SysLog 记录，
// 其余行正常拼接，不 panic。
func TestMergeConversationInvalidRowSkipped(t *testing.T) {
	var logBuffer bytes.Buffer
	common.LogWriterMu.Lock()
	previousWriter := gin.DefaultWriter
	gin.DefaultWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultWriter = previousWriter
		common.LogWriterMu.Unlock()
	})

	turns := []model.ConversationTurn{
		{Messages: `[{"role":"user","kind":"text","text":"a"}]`},
		{Messages: `not json`},
		{Messages: `[{"role":"assistant","kind":"text","text":"b"}]`},
	}
	got := MergeConversation(turns)
	require.Len(t, got, 2, "非法 JSON 行记空，其余两行正常拼接")
	assert.Equal(t, "a", got[0].Text)
	assert.Equal(t, "b", got[1].Text)
	assert.Contains(t, logBuffer.String(), "[SYS]", "解码失败必须经 SysLog 记录")
}

// TestMergeConversationEmptyInputs 边界：nil/空 turns、空 messages、null messages
// 均不 panic，结果为空或跳过。
func TestMergeConversationEmptyInputs(t *testing.T) {
	assert.Empty(t, MergeConversation(nil), "nil turns 记空")
	assert.Empty(t, MergeConversation([]model.ConversationTurn{}), "空 turns 记空")
	assert.Empty(t, MergeConversation([]model.ConversationTurn{{Messages: ""}}), "空 messages 解码失败记空")
	assert.Empty(t, MergeConversation([]model.ConversationTurn{{Messages: "null"}}), "null messages 解码为空切片")
}

// setupServiceConversationTestDB 替换 model.DB/model.LOG_DB 为 sqlite 内存库并建
// conversation_turns 表，t.Cleanup 恢复原值（仿 model 包 setupConversationTurnTestDB）。
func setupServiceConversationTestDB(t *testing.T) {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	t.Cleanup(func() { model.DB, model.LOG_DB = previousDB, previousLogDB })
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// 强制单连接：:memory: 库按连接隔离，异步写库 goroutine 与主测试 goroutine
	// 需共享同一连接才能看到同一张表。
	sqlDB.SetMaxOpenConns(1)
	model.DB, model.LOG_DB = db, db
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS conversation_turns (
		id INTEGER DEFAULT 0,
		session_key TEXT DEFAULT '',
		request_id TEXT DEFAULT '',
		created_at INTEGER DEFAULT 0,
		messages TEXT DEFAULT '',
		turn_kind TEXT DEFAULT 'normal',
		model_name TEXT DEFAULT '',
		channel_id INTEGER DEFAULT 0,
		token_id INTEGER DEFAULT 0,
		token_name TEXT DEFAULT '',
		user_id INTEGER DEFAULT 0,
		username TEXT DEFAULT '',
		"group" TEXT DEFAULT '',
		ip TEXT DEFAULT '',
		is_stream INTEGER DEFAULT 0,
		use_time INTEGER DEFAULT 0,
		upstream_request_id TEXT DEFAULT '',
		prompt_tokens INTEGER DEFAULT 0,
		completion_tokens INTEGER DEFAULT 0
	)`).Error)
}

// TestRecordConversationWritesTurn 集成断言：RecordConversation 异步编排解析→会话识别→
// 判定→组装→写库。首轮（isNew=true）存全量 requestParts+assistant，turn_kind=first；
// 续链（isNew=false）切增量（上一轮 assistant 之后）+ 本轮 assistant，system 与历史不重复，
// turn_kind=normal。token 来自响应 usage，维度字段透传 input 纯值快照。
func TestRecordConversationWritesTurn(t *testing.T) {
	setupServiceConversationTestDB(t)
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	sessionKeyLocalMap = sync.Map{}
	singleInstanceLogOnce = sync.Once{}
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	// 第一轮：system + user1。
	RecordConversation(ConversationInput{
		RequestID:         "req_1",
		CreatedAt:         1781234567,
		ModelName:         "gpt-4o",
		ChannelID:         1,
		TokenID:           2,
		TokenName:         "tk",
		UserID:            3,
		Username:          "u",
		Group:             "g",
		IP:                "1.2.3.4",
		UseTime:           120,
		IsStream:          false,
		UpstreamRequestID: "up_req",
		Path:              "/v1/chat/completions",
		RawRequestBody:    []byte(`{"messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}]}`),
		RawResponseBody:   []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`),
	})
	// 等第一轮写库完成（resolveSessionKey 已把会话 slot 落地），再发第二轮，保证续链
	// 判定读到 slot（两轮经 gopool.Go 异步执行，须显式同步顺序）。
	var turn1 model.ConversationTurn
	require.Eventually(t, func() bool {
		return model.LOG_DB.Table("conversation_turns").Where("request_id = ?", "req_1").First(&turn1).Error == nil
	}, 3*time.Second, 10*time.Millisecond, "第一轮异步写库应在超时内完成")

	// 第二轮：历史叠加（system + user1 + assistant(hi) + user2）。
	RecordConversation(ConversationInput{
		RequestID:       "req_2",
		CreatedAt:       1781234577,
		TokenID:         2,
		Path:            "/v1/chat/completions",
		RawRequestBody:  []byte(`{"messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"},{"role":"assistant","content":"hi"},{"role":"user","content":"第二问"}]}`),
		RawResponseBody: []byte(`{"choices":[{"message":{"content":"second"}}],"usage":{"prompt_tokens":20,"completion_tokens":6}}`),
	})

	var turn2 model.ConversationTurn
	require.Eventually(t, func() bool {
		return model.LOG_DB.Table("conversation_turns").Where("request_id = ?", "req_2").First(&turn2).Error == nil
	}, 3*time.Second, 10*time.Millisecond, "第二轮异步写库应在超时内完成")

	// 同会话续链，sessionKey 一致。
	assert.NotEmpty(t, turn1.SessionKey, "sessionKey 由 resolveSessionKey 生成")
	assert.Equal(t, turn1.SessionKey, turn2.SessionKey, "续链两轮 sessionKey 一致")
	assert.Equal(t, turnKindFirst, turn1.TurnKind, "首轮 first")
	assert.Equal(t, turnKindNormal, turn2.TurnKind, "续链 normal")
	assert.Equal(t, int64(1781234567), turn1.CreatedAt)
	assert.Equal(t, "gpt-4o", turn1.ModelName)
	assert.Equal(t, 1, turn1.ChannelId)
	assert.Equal(t, 2, turn1.TokenId)
	assert.Equal(t, "tk", turn1.TokenName)
	assert.Equal(t, 3, turn1.UserId)
	assert.Equal(t, "u", turn1.Username)
	assert.Equal(t, "g", turn1.Group)
	assert.Equal(t, "1.2.3.4", turn1.Ip)
	assert.Equal(t, false, turn1.IsStream)
	assert.Equal(t, 120, turn1.UseTime)
	assert.Equal(t, "up_req", turn1.UpstreamRequestId)
	assert.Equal(t, 10, turn1.PromptTokens, "token 来自响应 usage 解析")
	assert.Equal(t, 5, turn1.CompletionTokens)
	assert.Equal(t, 6, turn2.CompletionTokens)

	// 第一轮全量：system + user1 + assistant(hi)。
	var parts1 []MsgPart
	require.NoError(t, common.Unmarshal([]byte(turn1.Messages), &parts1))
	require.Len(t, parts1, 3, "首轮全量存 system+user+assistant")
	assert.Equal(t, msgRoleSystem, parts1[0].Role)
	assert.Equal(t, "你是助手", parts1[0].Text)
	assert.Equal(t, "你好", parts1[1].Text)
	assert.Equal(t, "hi", parts1[2].Text)

	// 第二轮增量：切点为上一轮 assistant 之后，仅 user2 + assistant(second)；system 与历史不重复。
	var parts2 []MsgPart
	require.NoError(t, common.Unmarshal([]byte(turn2.Messages), &parts2))
	require.Len(t, parts2, 2, "续链增量仅本轮 user + assistant")
	assert.Equal(t, "第二问", parts2[0].Text)
	assert.Equal(t, "second", parts2[1].Text)

	// MergeConversation 按行 append 拼完整对话，无重复无缺失。
	merged := MergeConversation([]model.ConversationTurn{turn1, turn2})
	require.Len(t, merged, 5)
	assert.Equal(t, "你是助手", merged[0].Text)
	assert.Equal(t, "你好", merged[1].Text)
	assert.Equal(t, "hi", merged[2].Text)
	assert.Equal(t, "第二问", merged[3].Text)
	assert.Equal(t, "second", merged[4].Text)
}

// TestRecordConversationToolRound 集成断言：工具调用多轮增量链。首轮 user 发起，assistant
// 回 tool_use（首轮全量）；第二轮请求含历史 assistant(tool_use) + tool(result)，续链切增量
// （上一轮 assistant 之后）= [tool_result] + 本轮 assistant，turn_kind=tool_round；MergeConversation
// 拼接无重复。
func TestRecordConversationToolRound(t *testing.T) {
	setupServiceConversationTestDB(t)
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	sessionKeyLocalMap = sync.Map{}
	singleInstanceLogOnce = sync.Once{}
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	// 第一轮：user 查天气，assistant 回 tool_use。
	RecordConversation(ConversationInput{
		RequestID:       "req_tool_1",
		CreatedAt:       300,
		TokenID:         9,
		Path:            "/v1/chat/completions",
		RawRequestBody:  []byte(`{"messages":[{"role":"user","content":"查天气"}]}`),
		RawResponseBody: []byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]}}]}`),
	})
	// 等第一轮写库完成（会话 slot 落地），再发第二轮保证续链读到 slot。
	var turn1 model.ConversationTurn
	require.Eventually(t, func() bool {
		return model.LOG_DB.Table("conversation_turns").Where("request_id = ?", "req_tool_1").First(&turn1).Error == nil
	}, 3*time.Second, 10*time.Millisecond, "第一轮异步写库应在超时内完成")

	// 第二轮：历史 user + assistant(tool_use) + tool(result)，assistant 回总结。
	RecordConversation(ConversationInput{
		RequestID:       "req_tool_2",
		CreatedAt:       310,
		TokenID:         9,
		Path:            "/v1/chat/completions",
		RawRequestBody:  []byte(`{"messages":[{"role":"user","content":"查天气"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"晴"}]}`),
		RawResponseBody: []byte(`{"choices":[{"message":{"content":"今天晴"}}]}`),
	})

	var turn2 model.ConversationTurn
	require.Eventually(t, func() bool {
		return model.LOG_DB.Table("conversation_turns").Where("request_id = ?", "req_tool_2").First(&turn2).Error == nil
	}, 3*time.Second, 10*time.Millisecond, "第二轮异步写库应在超时内完成")

	assert.Equal(t, turn1.SessionKey, turn2.SessionKey, "工具多轮同会话")
	assert.Equal(t, turnKindToolRound, turn1.TurnKind, "首轮响应含 tool_use 记 tool_round")
	assert.Equal(t, turnKindToolRound, turn2.TurnKind, "续链轮含 tool_use/tool_result 记 tool_round")

	// 第一轮全量：user + assistant(tool_use)。
	var parts1 []MsgPart
	require.NoError(t, common.Unmarshal([]byte(turn1.Messages), &parts1))
	require.Len(t, parts1, 2)
	assert.Equal(t, "查天气", parts1[0].Text)
	assert.Equal(t, msgKindToolUse, parts1[1].Kind)

	// 第二轮增量：切点为上一轮 assistant(tool_use) 之后 = [tool_result] + 本轮 assistant。
	var parts2 []MsgPart
	require.NoError(t, common.Unmarshal([]byte(turn2.Messages), &parts2))
	require.Len(t, parts2, 2, "工具轮增量仅 tool_result + 本轮 assistant，历史不重复")
	assert.Equal(t, msgKindToolResult, parts2[0].Kind)
	assert.Equal(t, "今天晴", parts2[1].Text)

	// 拼接无重复：user + tool_use + tool_result + assistant。
	merged := MergeConversation([]model.ConversationTurn{turn1, turn2})
	require.Len(t, merged, 4)
}

// TestRecordConversationParseFailureStillRecords 边界：请求体非法 JSON 时 request 侧
// 记空，不中断写库，assistant 侧正常入库。
func TestRecordConversationParseFailureStillRecords(t *testing.T) {
	setupServiceConversationTestDB(t)
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	sessionKeyLocalMap = sync.Map{}
	singleInstanceLogOnce = sync.Once{}
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	RecordConversation(ConversationInput{
		RequestID:       "req_parse_fail",
		CreatedAt:       400,
		TokenID:         10,
		Path:            "/v1/chat/completions",
		RawRequestBody:  []byte(`{"messages":[`),
		RawResponseBody: []byte(`{"choices":[{"message":{"content":"x"}}]}`),
	})

	var turn model.ConversationTurn
	require.Eventually(t, func() bool {
		return model.LOG_DB.Table("conversation_turns").Where("request_id = ?", "req_parse_fail").First(&turn).Error == nil
	}, 3*time.Second, 10*time.Millisecond, "请求解析失败不中断写库")

	var parts []MsgPart
	require.NoError(t, common.Unmarshal([]byte(turn.Messages), &parts))
	require.Len(t, parts, 1, "request 侧记空，仅 assistant 侧消息")
	assert.Equal(t, msgRoleAssistant, parts[0].Role)
	assert.Equal(t, "x", parts[0].Text)
}

// lockedBuffer 为并发安全的日志缓冲：异步 goroutine 经 gin.DefaultWriter/
// DefaultErrorWriter 写入，测试轮询 String() 读取，两者共用同一把锁，避免
// bytes.Buffer 在读写并发下的 data race。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (lb *lockedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.b.Write(p)
}

func (lb *lockedBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.b.String()
}

// TestRecordConversationWriteFailureLogs 集成断言：写库失败经 model 层 common.SysError
// 记录，不 panic。日志断言只绑定 [SYS] 前缀，不绑定具体文案。
func TestRecordConversationWriteFailureLogs(t *testing.T) {
	setupServiceConversationTestDB(t)
	require.NoError(t, model.LOG_DB.Exec("DROP TABLE conversation_turns").Error)

	logBuffer := &lockedBuffer{}
	common.LogWriterMu.Lock()
	previousWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousWriter
		common.LogWriterMu.Unlock()
	})

	RecordConversation(ConversationInput{
		RequestID:       "req_fail",
		CreatedAt:       500,
		TokenID:         11,
		Path:            "/v1/chat/completions",
		RawRequestBody:  []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		RawResponseBody: []byte(`{"choices":[{"message":{"content":"x"}}]}`),
	})

	require.Eventually(t, func() bool {
		return strings.Contains(logBuffer.String(), "[SYS]")
	}, 3*time.Second, 10*time.Millisecond, "写库失败必须经 SysError 记录")
}

// TestResolveUsername 核心断言（Important #2）：Username 显式非空或 UserID 无效时
// 原样返回；Username 为空且 UserID 有效时经 model.GetUsernameById 回退解析真实用户名；
// 查无用户或查询失败时保留原值（空串），不 panic。
func TestResolveUsername(t *testing.T) {
	setupServiceConversationTestDB(t)
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	// GetUsernameById 走 model.DB（主库）查 users 表，User 结构体含软删除字段，
	// 查询自动带 deleted_at IS NULL 条件，表需含该列。
	require.NoError(t, model.DB.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, deleted_at DATETIME)`).Error)
	require.NoError(t, model.DB.Exec(`INSERT INTO users (id, username) VALUES (3, 'real_user')`).Error)

	assert.Equal(t, "explicit_user", resolveUsername("explicit_user", 3), "显式用户名非空时原样返回，不查询 DB")
	assert.Equal(t, "real_user", resolveUsername("", 3), "Username 为空且 UserID 有效时回退解析真实用户名")
	assert.Equal(t, "", resolveUsername("", 0), "UserID 无效（<=0）时保留原值")
	assert.Equal(t, "", resolveUsername("", 99), "查无用户名时保留原值（空串）")
}
