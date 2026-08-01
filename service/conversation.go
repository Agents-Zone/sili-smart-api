package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
)

// MsgPart 消息片段，kind 区分 text/tool_use/tool_result。
// Role 取值 user / assistant / tool / system。
type MsgPart struct {
	Role string `json:"role"`
	Kind string `json:"kind"`
	Text string `json:"text"`
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

// 协议判定常量，T3/T4 复用同一协议判定逻辑。
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

// parseRequestMessages 按 path 定协议，把请求体 messages 归一化成 []MsgPart。
// OpenAI 的 role=tool 标 Kind=tool_result；Claude 包在 user role content 里的
// tool_result block 也标 Kind=tool_result；首轮全部 request 侧消息按请求原序。
func parseRequestMessages(path string, body []byte) ([]MsgPart, error) {
	switch protocolForPath(path) {
	case openAIProtocol:
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
			parts = append(parts, MsgPart{Role: msgRoleTool, Kind: msgKindToolResult, Text: convMessageText(m.Content)})
			continue
		}
		if text := convMessageText(m.Content); text != "" {
			parts = append(parts, MsgPart{Role: m.Role, Kind: msgKindText, Text: text})
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "" {
				continue
			}
			parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolCallText(tc.Function.Name, tc.Function.Arguments)})
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
				parts = append(parts, MsgPart{Role: msgRoleTool, Kind: msgKindToolResult, Text: claudeBlockText(block["content"])})
			case "tool_use":
				if name, ok := block["name"].(string); ok && name != "" {
					parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolCallText(name, block["input"])})
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
					parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolCallText(part.FunctionCall.Name, part.FunctionCall.Args)})
				}
			case part.FunctionResponse != nil:
				if part.FunctionResponse.Name != "" {
					parts = append(parts, MsgPart{Role: msgRoleTool, Kind: msgKindToolResult, Text: compactJSONString(part.FunctionResponse.Response)})
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

// toolCallText 组装工具名与入参的紧凑表示，如 get_weather({"city":"北京"})。
// args 为 string（OpenAI 已是 JSON 串）时直接拼接，否则 JSON 序列化。
func toolCallText(name string, args any) string {
	return name + "(" + compactJSONString(args) + ")"
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

// parseAssistantContent 输出同结构 []MsgPart：text 段标 Kind=text，
// function_call/tool_use 标 Kind=tool_use。非流式解析 JSON（common.Unmarshal），
// 流式逐行扫描 SSE。失败/错误响应（上游 4xx/5xx JSON）无消息体时记空。
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
			parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolCallText(tc.Function.Name, tc.Function.Arguments)})
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
				parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolCallText(out.Name, out.Arguments)})
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
				parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolCallText(block.Name, block.Input)})
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

// accumToolCall 流式工具调用累积器，按 index 聚合 name 与增量 arguments。
type accumToolCall struct {
	name string
	args string
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
			acc.args += tc.Function.Arguments
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
				acc.args += ev.Delta
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
				acc.args += ev.Delta.PartialJSON
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
						toolParts = append(toolParts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: toolCallText(part.FunctionCall.Name, part.FunctionCall.Args)})
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

// assembleStreamParts 组装流式输出：join textParts 得到文本段，再按 index 升序
// 追加工具调用段（跳过空文本与无名称工具调用，name 为空视为无有效工具调用）。
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
		parts = append(parts, MsgPart{Role: msgRoleAssistant, Kind: msgKindToolUse, Text: tc.name + "(" + tc.args + ")"})
	}
	return parts
}

// sseDataLines 把流式响应体按行拆分，跳过 event: 行与 [DONE]，剥掉 data: 前缀，
// 返回每个 JSON 数据载荷。仿 relay/helper/stream_scanner.go 的逐行扫描模式。
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
		lines = append(lines, data)
	}
	return lines
}

// parseUsage 从响应字节解析 prompt/completion tokens，本表 token 唯一来源。
// 只读响应字节，不做计费运算。
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

// 会话识别缓存键前缀与 TTL（BR1）：键形如 conv:session:{token_id}:{prefixHash}，
// TTL 30min。
const (
	convSessionCachePrefix = "conv:session:"
	convSessionCacheTTL    = 30 * time.Minute
	convHashPrefixLen      = 64
)

// sessionKeyLocalMap 单实例退化模式的进程内会话映射：cacheKey -> sessionKey。
// singleInstanceLogOnce 保证「单实例模式」SysError 标注只输出一次。
var (
	sessionKeyLocalMap    sync.Map
	singleInstanceLogOnce sync.Once
)

// redisSetNX 原子抢占（SET NX EX），直连 common.RDB，不新增 common/redis.go 方法。
func redisSetNX(key, value string, ttl time.Duration) (bool, error) {
	ctx := context.Background()
	return common.RDB.SetNX(ctx, key, value, ttl).Result()
}

// redisExpire 命中即滚动续期 TTL。
func redisExpire(key string, ttl time.Duration) error {
	ctx := context.Background()
	return common.RDB.Expire(ctx, key, ttl).Err()
}

// combineRequestText 组合本轮 request 侧增量文本（BR2）：
//   - 含 tool_result 轮：全部 tool_result 文本 + 末条 user 文本（tool_result 在前）；
//   - 无 tool_result 且无 assistant 历史（新建会话首轮）：全部 user 侧文本按请求原序；
//   - 其余纯文本轮：末条 user 文本。
//
// 空 requestParts 返回空串，行为确定不 panic。
func combineRequestText(parts []MsgPart) string {
	var toolResults []string
	var userTexts []string
	lastUser := ""
	hasToolResult := false
	hasAssistant := false
	for _, p := range parts {
		switch {
		case p.Kind == msgKindToolResult:
			hasToolResult = true
			if p.Text != "" {
				toolResults = append(toolResults, p.Text)
			}
		case p.Kind == msgKindToolUse || p.Role == msgRoleAssistant:
			hasAssistant = true
		case p.Role == msgRoleUser && p.Text != "":
			userTexts = append(userTexts, p.Text)
			lastUser = p.Text
		}
	}
	switch {
	case hasToolResult:
		var b strings.Builder
		for _, t := range toolResults {
			b.WriteString(t)
		}
		b.WriteString(lastUser)
		return b.String()
	case hasAssistant:
		return lastUser
	default:
		return strings.Join(userTexts, "")
	}
}

// prefixHash 按 token_id 分桶计算本轮 request 侧内容的前缀指纹。
// 对组合后的 request 侧消息文本取前 64 字符，与 token_id 一起做 sha256 摘要，
// 同 token_id 同内容得同指纹，不同 token_id 分桶为不同指纹。
func prefixHash(tokenID int, requestParts []MsgPart) string {
	content := combineRequestText(requestParts)
	if len(content) > convHashPrefixLen {
		content = content[:convHashPrefixLen]
	}
	h := sha256.New()
	h.Write([]byte(strconv.Itoa(tokenID)))
	h.Write([]byte{':'})
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

// resolveSessionKey 会话指纹到 sessionKey 的映射，返回是否新建会话。
// Redis 可用走 Redis 映射（跨实例共享）；不可用退化进程内 map 单实例模式。
// Redis 运行期出错时捕获链路不中断，按新会话回退生成 sessionKey 并 SysError 记录。
func resolveSessionKey(tokenID int, requestParts []MsgPart) (sessionKey string, isNew bool) {
	key := convSessionCachePrefix + strconv.Itoa(tokenID) + ":" + prefixHash(tokenID, requestParts)

	if !common.RedisEnabled {
		singleInstanceLogOnce.Do(func() {
			common.SysError("conversation session cache: Redis disabled, running in single-instance mode (in-process session map); configure Redis for multi-instance session continuity")
		})
		if v, ok := sessionKeyLocalMap.Load(key); ok {
			return v.(string), false
		}
		sessionKey = common.NewRequestId()
		sessionKeyLocalMap.Store(key, sessionKey)
		return sessionKey, true
	}

	if v, err := common.RedisGet(key); err == nil && v != "" {
		_ = redisExpire(key, convSessionCacheTTL)
		return v, false
	} else if err != nil && !errors.Is(err, redis.Nil) {
		common.SysError("conversation session cache: Redis runtime error on get: " + err.Error())
		return common.NewRequestId(), true
	}

	sessionKey = common.NewRequestId()
	ok, err := redisSetNX(key, sessionKey, convSessionCacheTTL)
	if err != nil {
		common.SysError("conversation session cache: Redis runtime error on set nx: " + err.Error())
		return sessionKey, true
	}
	if ok {
		return sessionKey, true
	}
	if v, err := common.RedisGet(key); err == nil && v != "" {
		_ = redisExpire(key, convSessionCacheTTL)
		return v, false
	} else if err != nil && !errors.Is(err, redis.Nil) {
		common.SysError("conversation session cache: Redis runtime error on re-get: " + err.Error())
		return sessionKey, true
	}
	return sessionKey, true
}
