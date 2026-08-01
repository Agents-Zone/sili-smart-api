package service

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
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

// TestPrefixHash 核心断言：同 tokenID 同内容同指纹；同 tokenID 不同内容不同指纹；
// 不同 tokenID 同内容不同指纹（token_id 分桶）。
func TestPrefixHash(t *testing.T) {
	parts := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "你好"}}
	same := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "你好"}}
	different := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "今天天气怎么样"}}

	h1 := prefixHash(1, parts)
	h2 := prefixHash(1, same)
	h3 := prefixHash(1, different)
	h4 := prefixHash(2, same)

	assert.NotEmpty(t, h1)
	assert.Equal(t, h1, h2, "同 tokenID 同内容必须得到相同指纹")
	assert.NotEqual(t, h1, h3, "同 tokenID 不同内容必须得到不同指纹")
	assert.NotEqual(t, h1, h4, "不同 tokenID 同内容必须分桶为不同指纹")
}

// TestPrefixHashEmptyParts 空 requestParts（组合文本为空）不 panic，指纹确定。
func TestPrefixHashEmptyParts(t *testing.T) {
	h1 := prefixHash(1, nil)
	h2 := prefixHash(1, []MsgPart{})
	assert.NotEmpty(t, h1)
	assert.Equal(t, h1, h2, "空输入必须得到确定指纹")
	assert.NotEqual(t, h1, prefixHash(2, nil), "空输入也受 token_id 分桶")
}

// TestCombineRequestTextByContentType 内容组合规则（BR2）：纯文本轮末条 user；
// 工具轮全部 tool_result + 末条 user（tool_result 在前）；首轮全部 user 侧按原序。
func TestCombineRequestTextByContentType(t *testing.T) {
	pureTextTurn := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "第一问"},
		{Role: msgRoleAssistant, Kind: msgKindText, Text: "回答一"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "第二问"},
	}
	assert.Equal(t, "第二问", combineRequestText(pureTextTurn), "纯文本轮只取末条 user 文本")

	toolTurn := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "查天气"},
		{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: `get_weather({"city":"北京"})`},
		{Role: msgRoleTool, Kind: msgKindToolResult, Text: "晴，25度"},
		{Role: msgRoleTool, Kind: msgKindToolResult, Text: "风力3级"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "然后呢"},
	}
	assert.Equal(t, "晴，25度风力3级然后呢", combineRequestText(toolTurn), "工具轮为全部 tool_result + 末条 user，tool_result 在前")

	firstTurn := []MsgPart{
		{Role: msgRoleSystem, Kind: msgKindText, Text: "system prompt"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "你好"},
	}
	assert.Equal(t, "你好", combineRequestText(firstTurn), "首轮取全部 user 侧文本按原序")

	firstTurnMultiUser := []MsgPart{
		{Role: msgRoleUser, Kind: msgKindText, Text: "开场白"},
		{Role: msgRoleUser, Kind: msgKindText, Text: "补充问题"},
	}
	assert.Equal(t, "开场白补充问题", combineRequestText(firstTurnMultiUser), "首轮多条 user 按请求原序拼接")
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

// TestResolveSessionKeySingleInstanceDegraded 核心断言：Redis 不可用时（保存并恢复
// 原值）同 tokenID + 同 requestParts 连续两次返回相同 sessionKey，第一次 isNew=true、
// 第二次 isNew=false；不同内容返回不同 sessionKey；不同 tokenID 同内容分桶。
func TestResolveSessionKeySingleInstanceDegraded(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	sessionKeyLocalMap = sync.Map{}
	singleInstanceLogOnce = sync.Once{}
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	parts := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "你好"}}
	sk1, isNew1 := resolveSessionKey(1, parts)
	sk2, isNew2 := resolveSessionKey(1, parts)
	assert.NotEmpty(t, sk1)
	assert.True(t, isNew1, "首次调用必须判定为新会话")
	assert.Equal(t, sk1, sk2, "同 tokenID 同内容必须复用同一 sessionKey")
	assert.False(t, isNew2, "二次调用必须命中已有会话")

	skDifferent, isNewDifferent := resolveSessionKey(1, []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "不同内容"}})
	assert.NotEqual(t, sk1, skDifferent, "不同内容必须生成不同 sessionKey")
	assert.True(t, isNewDifferent)

	skOtherToken, _ := resolveSessionKey(2, parts)
	assert.NotEqual(t, sk1, skOtherToken, "不同 tokenID 同内容必须分桶为不同会话")
}

// TestResolveSessionKeyDegradedLogsSingleInstance Redis 未配置时首例退化必须经
// common.SysError 标注单实例模式（sync.Once 保护），即 BR4。
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

	_, _ = resolveSessionKey(7, []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "退化标注"}})
	assert.Contains(t, logBuffer.String(), "single-instance", "退化模式必须输出单实例标注日志")

	// sync.Once 保证只标注一次：再次调用不再新增日志
	logBuffer.Reset()
	_, _ = resolveSessionKey(8, []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "再次调用"}})
	assert.Empty(t, logBuffer.String(), "单实例标注必须只输出一次")
}

// TestResolveSessionKeyRedisHitMissRenew Redis 可用：未命中 SetNX 抢占，命中复用
// sessionKey 并滚动续期，缓存键形如 conv:session:{token_id}:{prefixHash}，TTL 30min。
func TestResolveSessionKeyRedisHitMissRenew(t *testing.T) {
	server := useIndependentSessionRedis(t)
	parts := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "你好"}}

	sk1, isNew1 := resolveSessionKey(1, parts)
	assert.True(t, isNew1)
	assert.NotEmpty(t, sk1)

	key := convSessionCachePrefix + "1:" + prefixHash(1, parts)
	val, err := server.Get(key)
	require.NoError(t, err)
	assert.Equal(t, sk1, val, "Redis 中必须缓存 sessionKey")
	ttl := server.TTL(key)
	assert.True(t, ttl > 0 && ttl <= convSessionCacheTTL, "缓存 TTL 应为 30min，实际 %v", ttl)

	sk2, isNew2 := resolveSessionKey(1, parts)
	assert.Equal(t, sk1, sk2, "命中必须复用 sessionKey")
	assert.False(t, isNew2)

	server.FastForward(29 * time.Minute)
	_, isNew3 := resolveSessionKey(1, parts)
	assert.False(t, isNew3, "TTL 内滚动续期后仍应命中")
	assert.True(t, server.TTL(key) > 29*time.Minute, "命中后 TTL 应被滚动续期")
}

// TestResolveSessionKeyRedisConcurrentFirstRequest 并发首请求：SetNX 原子抢占消除劈
// 会话窗口，所有并发方收敛到同一 sessionKey 且仅一个 isNew=true。
func TestResolveSessionKeyRedisConcurrentFirstRequest(t *testing.T) {
	useIndependentSessionRedis(t)
	parts := []MsgPart{{Role: msgRoleUser, Kind: msgKindText, Text: "并发首请求"}}

	const n = 12
	keys := make([]string, n)
	isNews := make([]bool, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			sk, isNew := resolveSessionKey(3, parts)
			keys[idx] = sk
			isNews[idx] = isNew
		}(i)
	}
	close(start)
	wg.Wait()

	require.NotEmpty(t, keys[0])
	newCount := 0
	for i := 0; i < n; i++ {
		assert.Equal(t, keys[0], keys[i], "并发首请求必须收敛到同一 sessionKey")
		if isNews[i] {
			newCount++
		}
	}
	assert.Equal(t, 1, newCount, "并发首请求只能有一个胜出者 isNew=true")
}

// TestResolveSessionKeyRedisRuntimeErrorFallback Redis 运行期故障（连接失败）时
// 捕获链路不中断，按新会话回退生成 sessionKey 并 SysError 记录（BR5）。
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
	assert.Contains(t, logBuffer.String(), "Redis runtime error", "运行期故障必须 SysError 记录")
}

// TestRedisSetNXAndExpire redisSetNX 为 SET NX EX 原子抢占（已存在则失败不改值），
// redisExpire 命中即滚动续期 TTL。
func TestRedisSetNXAndExpire(t *testing.T) {
	server := useIndependentSessionRedis(t)
	key := "conv:session:9:abcd"
	ttl := time.Minute

	ok, err := redisSetNX(key, "sk-1", ttl)
	require.NoError(t, err)
	assert.True(t, ok, "首次 SET NX 必须成功")
	assert.Equal(t, "sk-1", mustGetServer(t, server, key))
	assert.True(t, server.TTL(key) > 0, "SET NX 必须带上 EX TTL")

	ok, err = redisSetNX(key, "sk-2", ttl)
	require.NoError(t, err)
	assert.False(t, ok, "key 已存在时 SET NX 必须失败")
	assert.Equal(t, "sk-1", mustGetServer(t, server, key), "SET NX 失败不得覆盖已存值")

	server.FastForward(45 * time.Second)
	require.NoError(t, redisExpire(key, ttl))
	remaining := server.TTL(key)
	assert.True(t, remaining > 45*time.Second, "redisExpire 必须滚动续期 TTL，实际剩余 %v", remaining)
}

func mustGetServer(t *testing.T, server *miniredis.Miniredis, key string) string {
	t.Helper()
	val, err := server.Get(key)
	require.NoError(t, err)
	return val
}
