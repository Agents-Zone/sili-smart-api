package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			name: "openai_completions_no_messages",
			path: "/v1/completions",
			body: `{"model":"gpt-3.5-turbo","prompt":"hello"}`,
			want: []MsgPart{},
		},
		{
			name: "openai_responses_parses_messages",
			path: "/v1/responses",
			body: `{"model":"gpt-5","messages":[{"role":"tool","tool_call_id":"call_1","content":"晴"}]}`,
			want: []MsgPart{
				{Role: msgRoleTool, Kind: msgKindToolResult, Text: "晴"},
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
// tool_result block 均标 Kind=tool_result 且 Role=tool。
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
