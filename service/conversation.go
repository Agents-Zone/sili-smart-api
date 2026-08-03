package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/go-redis/redis/v8"
)

// MsgPart 消息片段，kind 区分 text/tool_use/tool_result。
// Role 取值 user / assistant / tool / system。Text 含义随 Kind 变化：text 存原文，
// tool_use 存元信息（工具名 + 参数字节数，如 Read args=234），tool_result 存元信息
// （工具名 + 结果字节数，协议拿不到工具名时退化为 tool_result result=<N>）。
type MsgPart struct {
	Role string `json:"role"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// ConversationInput 是一次请求/响应的纯值快照，由 middleware 构造、异步传入 RecordConversation。
type ConversationInput struct {
	SessionKey        string
	RequestID         string
	CreatedAt         int64
	Messages          []MsgPart // 由 RecordConversation 内部解析后填充
	TurnKind          string
	ModelName         string
	ChannelID         int
	TokenID           int
	TokenName         string
	UserID            int
	Username          string
	Group             string
	IP                string
	UseTime           int64
	IsStream          bool
	UpstreamRequestID string
	PromptTokens      int
	CompletionTokens  int
	// 解析输入（middleware 同步段捕获的原始数据）
	Path            string
	RawRequestBody  []byte
	RawResponseBody []byte
}

// 消息片段 kind 常量。
const (
	msgKindText       = "text"
	msgKindToolUse    = "tool_use"
	msgKindToolResult = "tool_result"
)

// 消息片段 role 常量。
const (
	msgRoleUser      = "user"
	msgRoleAssistant = "assistant"
	msgRoleTool      = "tool"
	msgRoleSystem    = "system"
)

// 轮次类型：first 新建会话首轮，normal 常规轮，tool_round 含工具调用轮（优先）。
const (
	turnKindFirst     = "first"
	turnKindNormal    = "normal"
	turnKindToolRound = "tool_round"
)

// 协议判定常量。
const (
	openAIProtocol = "openai"
	claudeProtocol = "claude"
	geminiProtocol = "gemini"
)

// protocolForPath 按 path 判定协议。OpenAI/Claude 精确匹配，Gemini 后缀匹配；
// 不在白名单内的路径返回空串。
func protocolForPath(path string) string {
	switch {
	case path == "/v1/chat/completions", path == "/v1/completions", path == "/v1/responses", path == "/v1/responses/compact":
		return openAIProtocol
	case path == "/v1/messages":
		return claudeProtocol
	case strings.HasSuffix(path, ":generateContent"), strings.HasSuffix(path, ":streamGenerateContent"):
		return geminiProtocol
	default:
		return ""
	}
}

// isResponsesPath 判断是否为 OpenAI Responses API 路径（输出结构为 output[]）。
func isResponsesPath(path string) bool {
	return path == "/v1/responses" || path == "/v1/responses/compact"
}

// parseRequestMessages 按 path 判定协议，把请求体消息归一化为 []MsgPart，按请求原序输出。
func parseRequestMessages(path string, body []byte) ([]MsgPart, error) {
	switch protocolForPath(path) {
	case openAIProtocol:
		if isResponsesPath(path) {
			return parseOpenAIResponsesInput(body)
		}
		return parseOpenAIRequestMessages(body)
	case claudeProtocol:
		return parseClaudeRequestMessages(body)
	case geminiProtocol:
		return parseGeminiRequestMessages(body)
	default:
		return []MsgPart{}, nil
	}
}

// convOpenAIRequestMessage OpenAI chat/completions 请求消息，content 可为
// 字符串或多模态数组，assistant 历史消息可携带 tool_calls。
type convOpenAIRequestMessage struct {
	Role      string            `json:"role"`
	Content   any               `json:"content"`
	ToolCalls []convRequestTool `json:"tool_calls"`
	// Name 防御性冗余：OpenAI tool 消息规范不含工具名字段，反序列化恒为空，
	// 仅用于与 Gemini 侧 toolResultMeta 调用保持同构。
	Name string `json:"name"`
}

type convRequestTool struct {
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func parseOpenAIRequestMessages(body []byte) ([]MsgPart, error) {
	var req struct {
		Messages []convOpenAIRequestMessage `json:"messages"`
	}
	if err := common.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	parts := make([]MsgPart, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == msgRoleTool {
			parts = append(parts, MsgPart{Role: msgRoleTool, Kind: msgKindToolResult, Text: toolResultMeta(m.Name, convMessageText(m.Content))})
			continue
		}
		if text := convMessageText(m.Content); text != "" {
			parts = append(parts, MsgPart{Role: m.Role, Kind: msgKindText, Text: text})
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "" {
				continue
			}
			parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolUseMeta(tc.Function.Name, len(tc.Function.Arguments))})
		}
	}
	return parts, nil
}

// parseOpenAIResponsesInput 解析 Responses API 的 input 字段（string 或数组形态）为 []MsgPart。
func parseOpenAIResponsesInput(body []byte) ([]MsgPart, error) {
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	if err := common.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		return []MsgPart{}, nil
	}
	var rawInput any
	if err := common.Unmarshal(req.Input, &rawInput); err != nil {
		return nil, err
	}
	var parts []MsgPart
	switch in := rawInput.(type) {
	case string:
		if in != "" {
			parts = append(parts, MsgPart{Role: msgRoleUser, Kind: msgKindText, Text: in})
		}
	case []any:
		for _, item := range in {
			if s, ok := item.(string); ok {
				if s != "" {
					parts = append(parts, MsgPart{Role: msgRoleUser, Kind: msgKindText, Text: s})
				}
				continue
			}
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			if role == msgRoleTool {
				parts = append(parts, MsgPart{Role: msgRoleTool, Kind: msgKindToolResult, Text: toolResultMeta("", convMessageText(m["content"]))})
				continue
			}
			if role == "" {
				role = msgRoleUser
			}
			if text := convMessageText(m["content"]); text != "" {
				parts = append(parts, MsgPart{Role: role, Kind: msgKindText, Text: text})
			}
		}
	}
	return parts, nil
}

// convClaudeRequestMessage Claude 请求消息，content 为字符串或 block 数组。
type convClaudeRequestMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func parseClaudeRequestMessages(body []byte) ([]MsgPart, error) {
	var req struct {
		Messages []convClaudeRequestMessage `json:"messages"`
	}
	if err := common.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	parts := make([]MsgPart, 0, len(req.Messages))
	for _, m := range req.Messages {
		if s, ok := m.Content.(string); ok {
			if s != "" {
				parts = append(parts, MsgPart{Role: m.Role, Kind: msgKindText, Text: s})
			}
			continue
		}
		blocks, ok := m.Content.([]any)
		if !ok {
			continue
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				if s, ok := block["text"].(string); ok && s != "" {
					parts = append(parts, MsgPart{Role: m.Role, Kind: msgKindText, Text: s})
				}
			case "tool_result":
				parts = append(parts, MsgPart{Role: msgRoleTool, Kind: msgKindToolResult, Text: toolResultMeta("", claudeBlockText(block["content"]))})
			case "tool_use":
				if name, ok := block["name"].(string); ok && name != "" {
					parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolUseMeta(name, anyLen(block["input"]))})
				}
			}
		}
	}
	return parts, nil
}

// convGeminiRequestPart Gemini content part，text/functionCall/functionResponse
// 三选一。
type convGeminiRequestPart struct {
	Text             string              `json:"text"`
	FunctionCall     *convGeminiFuncCall `json:"functionCall"`
	FunctionResponse *convGeminiFuncResp `json:"functionResponse"`
}

type convGeminiFuncCall struct {
	Name string `json:"name"`
	Args any    `json:"args"`
}

type convGeminiFuncResp struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

func parseGeminiRequestMessages(body []byte) ([]MsgPart, error) {
	var req struct {
		Contents []struct {
			Role  string                  `json:"role"`
			Parts []convGeminiRequestPart `json:"parts"`
		} `json:"contents"`
	}
	if err := common.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	parts := make([]MsgPart, 0, len(req.Contents))
	for _, content := range req.Contents {
		role := content.Role
		if role == "model" {
			role = msgRoleAssistant
		}
		for _, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				if part.FunctionCall.Name != "" {
					parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolUseMeta(part.FunctionCall.Name, anyLen(part.FunctionCall.Args))})
				}
			case part.FunctionResponse != nil:
				if part.FunctionResponse.Name != "" {
					parts = append(parts, MsgPart{Role: msgRoleTool, Kind: msgKindToolResult, Text: toolResultMeta(part.FunctionResponse.Name, compactJSONString(part.FunctionResponse.Response))})
				}
			case part.Text != "":
				parts = append(parts, MsgPart{Role: role, Kind: msgKindText, Text: part.Text})
			}
		}
	}
	return parts, nil
}

// convMessageText 提取 OpenAI/Claude 消息 content 字段的纯文本。content 可为
// string、[]any（多模态/block 数组）或 map；数组只拼接文本类 block。
func convMessageText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case nil:
		return ""
	case []any:
		var b strings.Builder
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ != "text" && typ != "input_text" && typ != "output_text" {
				continue
			}
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return compactJSONString(content)
	}
}

// claudeBlockText 提取 Claude tool_result block 的 content 文本。content 可为
// string 或 {type:text} block 数组。
func claudeBlockText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case nil:
		return ""
	case []any:
		var b strings.Builder
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return compactJSONString(content)
	}
}

// anyLen 返回任意 args 的 UTF-8 字节数：string 取 len，其它经 common.Marshal 取 len，nil/失败返回 0。
func anyLen(v any) int {
	switch s := v.(type) {
	case string:
		return len(s)
	case nil:
		return 0
	default:
		if data, err := common.Marshal(v); err == nil {
			return len(data)
		}
	}
	return 0
}

// toolUseMeta 组装 tool_use 元信息：工具名 + 参数字节数，如 get_weather args=18。
func toolUseMeta(name string, argsLen int) string {
	return name + " args=" + strconv.Itoa(argsLen)
}

// toolResultMeta 组装 tool_result 元信息：工具名（空则 "tool_result"）+ 结果字节数。
func toolResultMeta(name, text string) string {
	label := name
	if label == "" {
		label = "tool_result"
	}
	return label + " result=" + strconv.Itoa(len(text))
}

// compactJSONString 把任意 JSON 值转为紧凑字符串：string 原样返回，
// 其它值经 common.Marshal 序列化。
func compactJSONString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		if data, err := common.Marshal(v); err == nil {
			return string(data)
		}
	}
	return ""
}

// parseAssistantContent 解析响应体 assistant 输出为 []MsgPart：非流式解析 JSON，流式扫描 SSE；错误响应记空。
func parseAssistantContent(path string, isStream bool, respBytes []byte) []MsgPart {
	protocol := protocolForPath(path)
	if protocol == "" {
		return []MsgPart{}
	}
	if isStream {
		return parseAssistantStream(protocol, path, respBytes)
	}
	return parseAssistantNonStream(protocol, path, respBytes)
}

// parseAssistantNonStream 按 path 分派非流式 assistant 输出解析。
func parseAssistantNonStream(protocol, path string, respBytes []byte) []MsgPart {
	switch {
	case protocol == openAIProtocol && isResponsesPath(path):
		return parseOpenAIResponsesNonStream(respBytes)
	case protocol == openAIProtocol && path == "/v1/completions":
		return parseOpenAICompletionsNonStream(respBytes)
	case protocol == openAIProtocol:
		return parseOpenAIChatNonStream(respBytes)
	case protocol == claudeProtocol:
		return parseClaudeNonStream(respBytes)
	case protocol == geminiProtocol:
		return parseGeminiStreamOrNonStream(respBytes)
	default:
		return []MsgPart{}
	}
}

func parseOpenAIChatNonStream(respBytes []byte) []MsgPart {
	var resp struct {
		Choices []struct {
			Message struct {
				Content   any               `json:"content"`
				ToolCalls []convRequestTool `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := common.Unmarshal(respBytes, &resp); err != nil {
		return []MsgPart{}
	}
	parts := make([]MsgPart, 0, len(resp.Choices))
	for _, choice := range resp.Choices {
		if text := convMessageText(choice.Message.Content); text != "" {
			parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindText, Text: text})
		}
		for _, tc := range choice.Message.ToolCalls {
			if tc.Function.Name == "" {
				continue
			}
			parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolUseMeta(tc.Function.Name, len(tc.Function.Arguments))})
		}
	}
	return parts
}

func parseOpenAICompletionsNonStream(respBytes []byte) []MsgPart {
	var resp struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := common.Unmarshal(respBytes, &resp); err != nil {
		return []MsgPart{}
	}
	parts := make([]MsgPart, 0, len(resp.Choices))
	for _, choice := range resp.Choices {
		if choice.Text != "" {
			parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindText, Text: choice.Text})
		}
	}
	return parts
}

// convResponsesOutput OpenAI Responses 非流式输出项。
type convResponsesOutput struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
}

func parseOpenAIResponsesNonStream(respBytes []byte) []MsgPart {
	var resp struct {
		Output []convResponsesOutput `json:"output"`
	}
	if err := common.Unmarshal(respBytes, &resp); err != nil {
		return []MsgPart{}
	}
	parts := make([]MsgPart, 0, len(resp.Output))
	for _, out := range resp.Output {
		switch out.Type {
		case "message":
			for _, content := range out.Content {
				if (content.Type == "output_text" || content.Type == "text") && content.Text != "" {
					parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindText, Text: content.Text})
				}
			}
		case "function_call":
			if out.Name != "" {
				parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolUseMeta(out.Name, anyLen(out.Arguments))})
			}
		}
	}
	return parts
}

func parseClaudeNonStream(respBytes []byte) []MsgPart {
	var resp struct {
		Content []struct {
			Type  string  `json:"type"`
			Text  *string `json:"text"`
			Name  string  `json:"name"`
			Input any     `json:"input"`
		} `json:"content"`
	}
	if err := common.Unmarshal(respBytes, &resp); err != nil {
		return []MsgPart{}
	}
	parts := make([]MsgPart, 0, len(resp.Content))
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			if block.Text != nil && *block.Text != "" {
				parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindText, Text: *block.Text})
			}
		case "tool_use":
			if block.Name != "" {
				parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolUseMeta(block.Name, anyLen(block.Input))})
			}
		}
	}
	return parts
}

// parseAssistantStream 按 path 分派流式 assistant 输出解析。
func parseAssistantStream(protocol, path string, respBytes []byte) []MsgPart {
	switch {
	case protocol == openAIProtocol && isResponsesPath(path):
		return parseOpenAIResponsesStream(respBytes)
	case protocol == openAIProtocol:
		return parseOpenAIChatStream(respBytes)
	case protocol == claudeProtocol:
		return parseClaudeStream(respBytes)
	case protocol == geminiProtocol:
		return parseGeminiStreamOrNonStream(respBytes)
	default:
		return []MsgPart{}
	}
}

// accumToolCall 流式工具调用累积器，按 index 聚合 name 与增量 arguments 的字节数。
type accumToolCall struct {
	name    string
	argsLen int
}

// convOpenAIStreamChunk OpenAI chat/completions 与 completions 流式 chunk。
type convOpenAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          *string              `json:"content"`
			ReasoningContent *string              `json:"reasoning_content"`
			ToolCalls        []convStreamToolCall `json:"tool_calls"`
		} `json:"delta"`
		Text *string `json:"text"` // legacy completions 增量
	} `json:"choices"`
}

type convStreamToolCall struct {
	Index    int `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func parseOpenAIChatStream(respBytes []byte) []MsgPart {
	var textParts []string
	toolCalls := make(map[int]*accumToolCall)
	for _, data := range sseDataLines(respBytes) {
		var chunk convOpenAIStreamChunk
		if err := common.Unmarshal(data, &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != nil {
			textParts = append(textParts, *delta.Content)
		}
		if delta.ReasoningContent != nil {
			textParts = append(textParts, *delta.ReasoningContent)
		}
		if chunk.Choices[0].Text != nil {
			textParts = append(textParts, *chunk.Choices[0].Text)
		}
		for _, tc := range delta.ToolCalls {
			acc := toolCalls[tc.Index]
			if acc == nil {
				acc = &accumToolCall{}
				toolCalls[tc.Index] = acc
			}
			acc.name += tc.Function.Name
			acc.argsLen += len(tc.Function.Arguments)
		}
	}
	return assembleStreamParts(textParts, toolCalls)
}

// convResponsesStreamEvent OpenAI Responses 流式事件。
type convResponsesStreamEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	OutputIndex *int   `json:"output_index"`
	Item        struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"item"`
}

func parseOpenAIResponsesStream(respBytes []byte) []MsgPart {
	var textParts []string
	toolCalls := make(map[int]*accumToolCall)
	for _, data := range sseDataLines(respBytes) {
		var ev convResponsesStreamEvent
		if err := common.Unmarshal(data, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			textParts = append(textParts, ev.Delta)
		case "response.output_item.added":
			if ev.Item.Type == "function_call" && ev.OutputIndex != nil {
				acc := toolCalls[*ev.OutputIndex]
				if acc == nil {
					acc = &accumToolCall{}
					toolCalls[*ev.OutputIndex] = acc
				}
				acc.name = ev.Item.Name
			}
		case "response.function_call_arguments.delta":
			if ev.OutputIndex != nil {
				acc := toolCalls[*ev.OutputIndex]
				if acc == nil {
					acc = &accumToolCall{}
					toolCalls[*ev.OutputIndex] = acc
				}
				acc.argsLen += len(ev.Delta)
			}
		}
	}
	return assembleStreamParts(textParts, toolCalls)
}

// convClaudeStreamEvent Claude 流式事件。
type convClaudeStreamEvent struct {
	Type         string `json:"type"`
	Index        *int   `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

func parseClaudeStream(respBytes []byte) []MsgPart {
	var textParts []string
	toolCalls := make(map[int]*accumToolCall)
	for _, data := range sseDataLines(respBytes) {
		var ev convClaudeStreamEvent
		if err := common.Unmarshal(data, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" && ev.Index != nil {
				toolCalls[*ev.Index] = &accumToolCall{name: ev.ContentBlock.Name}
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				textParts = append(textParts, ev.Delta.Text)
			case "input_json_delta":
				idx := 0
				if ev.Index != nil {
					idx = *ev.Index
				}
				acc := toolCalls[idx]
				if acc == nil {
					acc = &accumToolCall{}
					toolCalls[idx] = acc
				}
				acc.argsLen += len(ev.Delta.PartialJSON)
			}
		}
	}
	return assembleStreamParts(textParts, toolCalls)
}

// parseGeminiStreamOrNonStream Gemini 非流式与流式均为
// candidates[].content.parts[] 结构，流式逐 chunk 累加文本。
func parseGeminiStreamOrNonStream(respBytes []byte) []MsgPart {
	lines := sseDataLines(respBytes)
	var textParts []string
	toolParts := make([]MsgPart, 0, 1)
	if len(lines) == 0 {
		// 非流式：整个响应体即一个 JSON 对象
		lines = [][]byte{respBytes}
	}
	for _, data := range lines {
		var chunk struct {
			Candidates []struct {
				Content struct {
					Role  string                  `json:"role"`
					Parts []convGeminiRequestPart `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := common.Unmarshal(data, &chunk); err != nil {
			continue
		}
		for _, cand := range chunk.Candidates {
			for _, part := range cand.Content.Parts {
				switch {
				case part.FunctionCall != nil:
					if part.FunctionCall.Name != "" {
						toolParts = append(toolParts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolUseMeta(part.FunctionCall.Name, anyLen(part.FunctionCall.Args))})
					}
				case part.Text != "":
					textParts = append(textParts, part.Text)
				}
			}
		}
	}
	parts := make([]MsgPart, 0, len(toolParts)+1)
	if text := strings.Join(textParts, ""); text != "" {
		parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindText, Text: text})
	}
	parts = append(parts, toolParts...)
	return parts
}

// sortedToolCallIndexes 返回 toolCalls 的 index 升序列表，保证输出顺序稳定。
func sortedToolCallIndexes(toolCalls map[int]*accumToolCall) []int {
	indexes := make([]int, 0, len(toolCalls))
	for idx := range toolCalls {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	return indexes
}

// assembleStreamParts 组装流式输出：拼接文本段，再按 index 升序追加工具调用段。
func assembleStreamParts(textParts []string, toolCalls map[int]*accumToolCall) []MsgPart {
	parts := make([]MsgPart, 0, len(toolCalls)+1)
	if text := strings.Join(textParts, ""); text != "" {
		parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindText, Text: text})
	}
	for _, idx := range sortedToolCallIndexes(toolCalls) {
		tc := toolCalls[idx]
		if tc.name == "" {
			continue
		}
		parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolUseMeta(tc.name, tc.argsLen)})
	}
	return parts
}

// sseDataLines 按行拆分流式响应体，跳过 event: 行与 [DONE]，剥掉 data: 前缀，返回每个 JSON 载荷。
// 每个 data 必须 bytes.Clone：Scanner.Bytes() 指向内部 buffer，响应体超过 64KB 时会被下一次 Fill 覆盖，调用方拿到损坏内容。
func sseDataLines(respBytes []byte) [][]byte {
	var lines [][]byte
	scanner := bufio.NewScanner(bytes.NewReader(respBytes))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 || bytes.HasPrefix(data, []byte("[DONE]")) {
			continue
		}
		lines = append(lines, bytes.Clone(data))
	}
	return lines
}

// parseUsage 从响应字节解析 prompt/completion tokens。
func parseUsage(path string, isStream bool, respBytes []byte) (promptTokens, completionTokens int) {
	protocol := protocolForPath(path)
	if protocol == "" {
		return 0, 0
	}
	switch {
	case protocol == openAIProtocol && isResponsesPath(path) && isStream:
		return parseResponsesStreamUsage(respBytes)
	case protocol == openAIProtocol && isStream:
		return parseOpenAIStreamUsage(respBytes)
	case protocol == openAIProtocol:
		return parseOpenAINonStreamUsage(respBytes)
	case protocol == claudeProtocol && isStream:
		return parseClaudeStreamUsage(respBytes)
	case protocol == claudeProtocol:
		return parseClaudeNonStreamUsage(respBytes)
	case protocol == geminiProtocol && isStream:
		return parseGeminiStreamUsage(respBytes)
	case protocol == geminiProtocol:
		return parseGeminiNonStreamUsage(respBytes)
	default:
		return 0, 0
	}
}

// convUsage OpenAI 系 usage，兼容 prompt_tokens/completion_tokens 与
// input_tokens/output_tokens（Responses API）两套字段命名。
type convUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

func (u *convUsage) prompt() int {
	if u.PromptTokens > 0 {
		return u.PromptTokens
	}
	return u.InputTokens
}

func (u *convUsage) completion() int {
	if u.CompletionTokens > 0 {
		return u.CompletionTokens
	}
	return u.OutputTokens
}

// convClaudeUsage Claude usage，input_tokens/output_tokens。
type convClaudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func parseOpenAINonStreamUsage(respBytes []byte) (int, int) {
	var resp struct {
		Usage *convUsage `json:"usage"`
	}
	if err := common.Unmarshal(respBytes, &resp); err != nil || resp.Usage == nil {
		return 0, 0
	}
	return resp.Usage.prompt(), resp.Usage.completion()
}

func parseOpenAIStreamUsage(respBytes []byte) (int, int) {
	var last *convUsage
	for _, data := range sseDataLines(respBytes) {
		var chunk struct {
			Usage *convUsage `json:"usage"`
		}
		if err := common.Unmarshal(data, &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			last = chunk.Usage
		}
	}
	if last == nil {
		return 0, 0
	}
	return last.prompt(), last.completion()
}

func parseResponsesStreamUsage(respBytes []byte) (int, int) {
	for _, data := range sseDataLines(respBytes) {
		var ev struct {
			Type     string `json:"type"`
			Response *struct {
				Usage *convUsage `json:"usage"`
			} `json:"response"`
		}
		if err := common.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.Type == "response.completed" && ev.Response != nil && ev.Response.Usage != nil {
			return ev.Response.Usage.prompt(), ev.Response.Usage.completion()
		}
	}
	return 0, 0
}

func parseClaudeNonStreamUsage(respBytes []byte) (int, int) {
	var resp struct {
		Usage *convClaudeUsage `json:"usage"`
	}
	if err := common.Unmarshal(respBytes, &resp); err != nil || resp.Usage == nil {
		return 0, 0
	}
	return resp.Usage.InputTokens, resp.Usage.OutputTokens
}

// parseClaudeStreamUsage 合并 message_start（message.usage）与 message_delta
// （usage）两处 input_tokens/output_tokens。
func parseClaudeStreamUsage(respBytes []byte) (int, int) {
	prompt, completion := 0, 0
	for _, data := range sseDataLines(respBytes) {
		var ev struct {
			Type    string `json:"type"`
			Message *struct {
				Usage *convClaudeUsage `json:"usage"`
			} `json:"message"`
			Usage *convClaudeUsage `json:"usage"`
		}
		if err := common.Unmarshal(data, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				if ev.Message.Usage.InputTokens > 0 {
					prompt = ev.Message.Usage.InputTokens
				}
				if ev.Message.Usage.OutputTokens > 0 {
					completion = ev.Message.Usage.OutputTokens
				}
			}
		case "message_delta":
			if ev.Usage != nil {
				if ev.Usage.InputTokens > 0 {
					prompt = ev.Usage.InputTokens
				}
				if ev.Usage.OutputTokens > 0 {
					completion = ev.Usage.OutputTokens
				}
			}
		}
	}
	return prompt, completion
}

// convGeminiUsageMetadata Gemini usageMetadata，promptTokenCount/candidatesTokenCount。
type convGeminiUsageMetadata struct {
	PromptTokens     int `json:"promptTokenCount"`
	CandidatesTokens int `json:"candidatesTokenCount"`
}

func parseGeminiNonStreamUsage(respBytes []byte) (int, int) {
	var resp struct {
		UsageMetadata *convGeminiUsageMetadata `json:"usageMetadata"`
	}
	if err := common.Unmarshal(respBytes, &resp); err != nil || resp.UsageMetadata == nil {
		return 0, 0
	}
	return resp.UsageMetadata.PromptTokens, resp.UsageMetadata.CandidatesTokens
}

// parseGeminiStreamUsage 取末个带 usageMetadata 的 chunk。
func parseGeminiStreamUsage(respBytes []byte) (int, int) {
	var last *convGeminiUsageMetadata
	for _, data := range sseDataLines(respBytes) {
		var chunk struct {
			UsageMetadata *convGeminiUsageMetadata `json:"usageMetadata"`
		}
		if err := common.Unmarshal(data, &chunk); err != nil {
			continue
		}
		if chunk.UsageMetadata != nil {
			last = chunk.UsageMetadata
		}
	}
	if last == nil {
		return 0, 0
	}
	return last.PromptTokens, last.CandidatesTokens
}

// 会话识别缓存：键 conv:session:{token_id}，value 为多槽 JSON 数组，TTL 30min。
const (
	convSessionCachePrefix = "conv:session:"
	convSessionCacheTTL    = 30 * time.Minute
	convSessionSlotCap     = 32
	convSessionSlotTimeout = 30 * time.Minute
)

// sessionKeyLocalMap：Redis 不可用时的进程内会话映射（单实例退化模式）。
var (
	sessionKeyLocalMap    sync.Map
	singleInstanceLogOnce sync.Once
)

// sessionSlot 记录一个会话最近一轮的指纹、request 条数与活跃时间，供续链比对与超时淘汰。
type sessionSlot struct {
	Fingerprint string `json:"fingerprint"`
	SessionKey  string `json:"session_key"`
	Count       int    `json:"count"`
	ActiveTime  int64  `json:"active_time"`
}

// sessionMultiSlot：一个 token 下的多会话槽位列表，退化模式下用 sync.Mutex 保护读改写。
type sessionMultiSlot struct {
	mu    sync.Mutex
	slots []sessionSlot
}

// redisExpire 滚动续期 TTL，当前未用，保留供后续原子化增强复用。
func redisExpire(key string, ttl time.Duration) error {
	ctx := context.Background()
	return common.RDB.Expire(ctx, key, ttl).Err()
}

// fingerprintMessages 对 []MsgPart 整体序列化做 sha256，作为会话续链的前缀比对基准。
func fingerprintMessages(parts []MsgPart) string {
	if parts == nil {
		parts = []MsgPart{}
	}
	data, err := common.Marshal(parts)
	if err != nil {
		// MsgPart 字段均为基本类型，Marshal 失败极罕见；兜底按空切片指纹，避免 panic。
		data = nil
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pruneExpiredSlots 淘汰超时 slot。
func pruneExpiredSlots(slots []sessionSlot, now int64) []sessionSlot {
	deadline := now - int64(convSessionSlotTimeout/time.Second)
	kept := make([]sessionSlot, 0, len(slots))
	for _, s := range slots {
		if s.ActiveTime >= deadline {
			kept = append(kept, s)
		}
	}
	return kept
}

// enforceSlotCap 多槽数组超上限时按 ActiveTime 最早淘汰。
func enforceSlotCap(slots []sessionSlot) []sessionSlot {
	for len(slots) > convSessionSlotCap {
		oldest := 0
		for i := 1; i < len(slots); i++ {
			if slots[i].ActiveTime < slots[oldest].ActiveTime {
				oldest = i
			}
		}
		slots = append(slots[:oldest], slots[oldest+1:]...)
	}
	return slots
}

// matchSessionSlots 在多槽列表中找续链 slot 或新建 slot（纯函数）。续链条件：本轮条数严格大于
// slot.Count，且前 slot.Count 条指纹相等（请求体完整包含上一轮 request 侧内容）。
func matchSessionSlots(slots []sessionSlot, requestParts []MsgPart, now int64) (string, bool, []sessionSlot) {
	slots = pruneExpiredSlots(slots, now)
	thisFp := fingerprintMessages(requestParts)
	thisCount := len(requestParts)
	for i := range slots {
		s := &slots[i]
		if thisCount > s.Count && fingerprintMessages(requestParts[:s.Count]) == s.Fingerprint {
			s.Fingerprint = thisFp
			s.Count = thisCount
			s.ActiveTime = now
			return s.SessionKey, false, slots
		}
	}
	sessionKey := common.NewRequestId()
	slots = append(slots, sessionSlot{
		Fingerprint: thisFp,
		SessionKey:  sessionKey,
		Count:       thisCount,
		ActiveTime:  now,
	})
	slots = enforceSlotCap(slots)
	return sessionKey, true, slots
}

// loadSessionSlots 从 Redis 读取 token 多槽列表，key 不存在或空串返回 nil。
func loadSessionSlots(key string) ([]sessionSlot, error) {
	val, err := common.RedisGet(key)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	if val == "" {
		return nil, nil
	}
	var slots []sessionSlot
	if err := common.UnmarshalJsonStr(val, &slots); err != nil {
		return nil, err
	}
	return slots, nil
}

// saveSessionSlots 写回 token 多槽列表，SET 带 TTL 天然滚动续期。
func saveSessionSlots(key string, slots []sessionSlot) error {
	data, err := common.Marshal(slots)
	if err != nil {
		return err
	}
	return common.RedisSet(key, string(data), convSessionCacheTTL)
}

// resolveSessionKey 会话识别：按 token 分桶做多槽前缀匹配，返回 sessionKey 与是否新建。
// Redis 不可用则退化进程内模式，运行期出错按新会话回退并 SysError 记录。
func resolveSessionKey(tokenID int, requestParts []MsgPart) (sessionKey string, isNew bool) {
	key := convSessionCachePrefix + strconv.Itoa(tokenID)
	now := time.Now().Unix()

	if !common.RedisEnabled {
		singleInstanceLogOnce.Do(func() {
			common.SysError("conversation session cache: Redis disabled, running in single-instance mode (in-process session map); configure Redis for multi-instance session continuity")
		})
		return resolveSessionKeyLocal(key, requestParts, now)
	}

	slots, err := loadSessionSlots(key)
	if err != nil {
		common.SysError("conversation session cache: Redis runtime error on load: " + err.Error())
		return common.NewRequestId(), true
	}
	sessionKey, isNew, newSlots := matchSessionSlots(slots, requestParts, now)
	if err := saveSessionSlots(key, newSlots); err != nil {
		common.SysError("conversation session cache: Redis runtime error on save: " + err.Error())
	}
	return sessionKey, isNew
}

// resolveSessionKeyLocal 退化模式：进程内 *sessionMultiSlot + Mutex 保护读改写。
func resolveSessionKeyLocal(key string, requestParts []MsgPart, now int64) (string, bool) {
	actual, _ := sessionKeyLocalMap.LoadOrStore(key, &sessionMultiSlot{})
	multi := actual.(*sessionMultiSlot)
	multi.mu.Lock()
	defer multi.mu.Unlock()
	sessionKey, isNew, newSlots := matchSessionSlots(multi.slots, requestParts, now)
	multi.slots = newSlots
	return sessionKey, isNew
}

// turnKindFor 判定轮次类型：含工具调用记 tool_round（优先），否则新建记 first，否则 normal。
func turnKindFor(parts []MsgPart, isNew bool) string {
	for _, p := range parts {
		if p.Kind == msgKindToolUse || p.Kind == msgKindToolResult {
			return turnKindToolRound
		}
	}
	if isNew {
		return turnKindFirst
	}
	return turnKindNormal
}

// lastAssistantIndex 返回 requestParts 中最后一个 role=assistant 的位置，找不到返回 -1。
func lastAssistantIndex(parts []MsgPart) int {
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i].Role == msgRoleAssistant {
			return i
		}
	}
	return -1
}

// joinConversationParts 按 isNew 决定存盘 messages：新建存全量，续链切增量（上一轮 assistant 之后）。
// isNew 同时驱动 sessionKey 新建/续用与全量/增量存储，使每个 session_key 形成首行全量、后续增量。
func joinConversationParts(requestParts, assistantParts []MsgPart, isNew bool) []MsgPart {
	if isNew {
		return append(requestParts, assistantParts...)
	}
	idx := lastAssistantIndex(requestParts)
	if idx < 0 {
		return append(requestParts, assistantParts...)
	}
	increment := requestParts[idx+1:]
	return append(increment, assistantParts...)
}

// MergeConversation 按传入顺序逐行 append 每行 messages，拼成完整 []MsgPart。解码失败的行跳过。
func MergeConversation(turns []model.ConversationTurn) []MsgPart {
	merged := make([]MsgPart, 0, len(turns)*2)
	for i := range turns {
		var parts []MsgPart
		if err := common.Unmarshal([]byte(turns[i].Messages), &parts); err != nil {
			common.SysLog("conversation: skip invalid messages row: " + err.Error())
			continue
		}
		merged = append(merged, parts...)
	}
	return merged
}

// resolveUsername 解析 username：显式非空或 UserID 无效时原样返回，否则按 UserID 回退查询真实用户名。
func resolveUsername(username string, userID int) string {
	if username != "" || userID <= 0 {
		return username
	}
	resolved, err := model.GetUsernameById(userID, false)
	if err != nil || resolved == "" {
		return username
	}
	return resolved
}

// 判断一次请求是否应记录到 conversation_turns。
// channel_id==0 表示请求未成功分发到上游渠道（在 Distribute 之前失败），
// 这类失败请求无会话价值，不记录。当前仅对 Claude 协议生效。
func shouldRecordConversation(input ConversationInput) bool {
	if input.ChannelID == 0 && protocolForPath(input.Path) == claudeProtocol {
		return false
	}
	return true
}

// RecordConversation 异步记录一次对话轮次：解析请求/响应 → 会话识别 → 组装 messages → 写库。
// 闭包只捕获 input 纯值快照，不阻塞请求；messages 按 isNew 存全量或增量。
func RecordConversation(input ConversationInput) {
	gopool.Go(func() {
		if !shouldRecordConversation(input) {
			return
		}
		requestParts, reqErr := parseRequestMessages(input.Path, input.RawRequestBody)
		if reqErr != nil {
			requestParts = []MsgPart{}
			common.SysLog("conversation: failed to parse request messages: " + reqErr.Error())
		}
		assistantParts := parseAssistantContent(input.Path, input.IsStream, input.RawResponseBody)
		promptTokens, completionTokens := parseUsage(input.Path, input.IsStream, input.RawResponseBody)
		sessionKey, isNew := resolveSessionKey(input.TokenID, requestParts)

		joinedParts := joinConversationParts(requestParts, assistantParts, isNew)
		messagesJSON, err := common.Marshal(joinedParts)
		if err != nil {
			common.SysLog("conversation: failed to marshal messages: " + err.Error())
			return
		}

		turn := &model.ConversationTurn{
			SessionKey:        sessionKey,
			RequestId:         input.RequestID,
			CreatedAt:         input.CreatedAt,
			Messages:          string(messagesJSON),
			TurnKind:          turnKindFor(joinedParts, isNew),
			ModelName:         input.ModelName,
			ChannelId:         input.ChannelID,
			TokenId:           input.TokenID,
			TokenName:         input.TokenName,
			UserId:            input.UserID,
			Username:          resolveUsername(input.Username, input.UserID),
			Group:             input.Group,
			Ip:                input.IP,
			IsStream:          input.IsStream,
			UseTime:           int(input.UseTime),
			UpstreamRequestId: input.UpstreamRequestID,
			PromptTokens:      promptTokens,
			CompletionTokens:  completionTokens,
		}
		// 写库失败由 RecordConversationTurn 内部记录，此处不重复 SysLog。
		model.RecordConversationTurn(turn)
	})
}
