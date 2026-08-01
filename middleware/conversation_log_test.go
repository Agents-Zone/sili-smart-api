package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// TestIsConversationPath 路径白名单（BR4）：OpenAI/Claude 精确匹配、Gemini 后缀匹配；
// 不覆盖 /v1/realtime、转写/TTS/embedding/image/rerank 等非对话路径。
func TestIsConversationPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		// 白名单内精确路径
		{"openai_chat_completions", "/v1/chat/completions", true},
		{"openai_completions", "/v1/completions", true},
		{"openai_responses", "/v1/responses", true},
		{"openai_responses_compact", "/v1/responses/compact", true},
		{"claude_messages", "/v1/messages", true},
		// Gemini 后缀匹配（覆盖 /v1beta 与 /v1 两个入口）
		{"gemini_generate_content_beta", "/v1beta/models/gemini-1.5-pro:generateContent", true},
		{"gemini_stream_generate_content_v1", "/v1/models/gemini:streamGenerateContent", true},
		// 白名单外路径
		{"embeddings", "/v1/embeddings", false},
		{"audio_transcriptions", "/v1/audio/transcriptions", false},
		{"audio_speech_tts", "/v1/audio/speech", false},
		{"image_generations", "/v1/images/generations", false},
		{"rerank", "/v1/rerank", false},
		{"realtime_websocket", "/v1/realtime", false},
		// 精确匹配不被前缀子路径误命中
		{"chat_completions_subpath", "/v1/chat/completions/extra", false},
		{"messages_subpath", "/v1/messages/extra", false},
		// Gemini 后缀不匹配（缺少 :generateContent/:streamGenerateContent）
		{"gemini_generate_only", "/v1beta/models/gemini-1.5-pro:generate", false},
		{"gemini_plain_model", "/v1beta/models/gemini-1.5-pro", false},
		{"empty_path", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isConversationPath(tt.path))
		})
	}
}

// TestConversationResponseWriter 响应捕获包装（BR3）：maxSize=0（默认无限）全量缓存不丢；
// maxSize>0（安全阀）时超限丢弃并累计 droppedBytes；始终透传底层 writer。
func TestConversationResponseWriter(t *testing.T) {
	tests := []struct {
		name        string
		maxSize     int
		writes      []string
		wantBody    string
		wantDropped int64
	}{
		// maxSize=0（默认无限）：全量缓存，无丢弃。
		{
			name:        "unlimited_single_write",
			maxSize:     0,
			writes:      []string{"1234567890123456789012345"},
			wantBody:    "1234567890123456789012345",
			wantDropped: 0,
		},
		{
			name:        "unlimited_multiple_writes",
			maxSize:     0,
			writes:      []string{"12345", "12345678901234567890", "abcdef"},
			wantBody:    "1234512345678901234567890abcdef",
			wantDropped: 0,
		},
		// maxSize>0（安全阀）：超限丢弃并累计 droppedBytes。
		{
			name:        "limited_single_write_exceeds",
			maxSize:     10,
			writes:      []string{"1234567890123456789012345"},
			wantBody:    "1234567890",
			wantDropped: 15,
		},
		{
			name:        "limited_multiple_writes_cross_limit",
			maxSize:     10,
			writes:      []string{"12345", "12345678901234567890"},
			wantBody:    "1234512345",
			wantDropped: 15,
		},
		{
			name:        "limited_write_after_full_all_dropped",
			maxSize:     10,
			writes:      []string{"1234567890", "abcdef"},
			wantBody:    "1234567890",
			wantDropped: 6,
		},
		{
			name:        "limited_exactly_at_limit_no_drop",
			maxSize:     10,
			writes:      []string{"1234567890"},
			wantBody:    "1234567890",
			wantDropped: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(rec)
			w := &conversationResponseWriter{
				ResponseWriter: ginCtx.Writer,
				body:           bytes.NewBuffer(nil),
				maxSize:        tt.maxSize,
			}
			wantTotal := 0
			for _, s := range tt.writes {
				n, err := w.WriteString(s)
				require.NoError(t, err)
				wantTotal += len(s)
				assert.Equal(t, len(s), n, "WriteString 返回透传字节数")
			}
			assert.Equal(t, tt.wantBody, w.body.String(), "body 缓冲区内容")
			assert.Equal(t, tt.wantDropped, w.droppedBytes, "droppedBytes 累计丢弃字节数")
			// 始终透传：底层 ResponseRecorder 收到完整字节。
			assert.Equal(t, wantTotal, rec.Body.Len(), "底层 writer 透传完整字节")
		})
	}
}

// TestConversationLogDisabled 开关关闭（BR1）：命中白名单路径直接 c.Next()，不包装
// writer、不读 body、不建表。
func TestConversationLogDisabled(t *testing.T) {
	t.Setenv("CONVERSATION_LOG_ENABLED", "")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	originalWriter := c.Writer

	ConversationLog()(c)

	assert.Same(t, originalWriter, c.Writer, "开关关闭时不得替换 c.Writer")
	_, ok := c.Writer.(*conversationResponseWriter)
	assert.False(t, ok, "开关关闭时 c.Writer 不得被包装为 conversationResponseWriter")
}

// TestConversationLogEnabledPathFiltered 开启但路径不在白名单（BR4）：直接 c.Next()，
// 不包装 writer、不建表、不读 body。
func TestConversationLogEnabledPathFiltered(t *testing.T) {
	t.Setenv("CONVERSATION_LOG_ENABLED", "true")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil)
	originalWriter := c.Writer

	ConversationLog()(c)

	assert.Same(t, originalWriter, c.Writer, "白名单外路径不得包装 c.Writer")
	_, ok := c.Writer.(*conversationResponseWriter)
	assert.False(t, ok, "白名单外路径 c.Writer 不得被包装为 conversationResponseWriter")
}

// TestConversationLogEnabledCaptures 开启且命中白名单（BR2/BR5）：包装 writer 捕获
// 响应体，同步段构造纯值快照，gopool 异步写库最终完成。
func TestConversationLogEnabledCaptures(t *testing.T) {
	setupConversationLogTestDB(t)
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	t.Setenv("CONVERSATION_LOG_ENABLED", "true")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/chat/completions", ConversationLog(), func(c *gin.Context) {
		// 模拟 Distribute 在 c.Next() 前写入的 context 值。
		common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4o")
		common.SetContextKey(c, constant.ContextKeyChannelId, 1)
		common.SetContextKey(c, constant.ContextKeyTokenId, 2)
		common.SetContextKey(c, constant.ContextKeyUserId, 3)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "g")
		common.SetContextKey(c, constant.ContextKeyIsStream, false)
		c.Set("token_name", "tk")
		c.Set("username", "u")
		c.Set(common.RequestIdKey, "req_mw_1")
		c.Set(common.UpstreamRequestIdKey, "up_req_mw_1")
		c.String(http.StatusOK, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "响应透传成功")

	var turn model.ConversationTurn
	require.Eventually(t, func() bool {
		return model.LOG_DB.Table("conversation_turns").Where("request_id = ?", "req_mw_1").First(&turn).Error == nil
	}, 3*time.Second, 10*time.Millisecond, "异步写库应在超时内完成")

	assert.NotEmpty(t, turn.SessionKey, "sessionKey 由 resolveSessionKey 生成")
	assert.NotEmpty(t, turn.Messages, "messages 含请求侧与响应侧消息")
	assert.Equal(t, "gpt-4o", turn.ModelName)
	assert.Equal(t, 1, turn.ChannelId)
	assert.Equal(t, 2, turn.TokenId)
	assert.Equal(t, "tk", turn.TokenName)
	assert.Equal(t, 3, turn.UserId)
	assert.Equal(t, "u", turn.Username)
	assert.Equal(t, "g", turn.Group)
	assert.Equal(t, "up_req_mw_1", turn.UpstreamRequestId)
	assert.Equal(t, 10, turn.PromptTokens)
	assert.Equal(t, 5, turn.CompletionTokens)
}

// TestConversationLogEnabledCapturesStream 开启且命中白名单的流式响应捕获（BR2/BR3）：
// IsStream=true 经 ConversationInput 透传落库，SSE 流式 assistant 内容正确解析为请求侧+
// 响应侧消息，usage chunk 的 token 正常落库。
func TestConversationLogEnabledCapturesStream(t *testing.T) {
	setupConversationLogTestDB(t)
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	t.Setenv("CONVERSATION_LOG_ENABLED", "true")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/chat/completions", ConversationLog(), func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4o")
		common.SetContextKey(c, constant.ContextKeyChannelId, 1)
		common.SetContextKey(c, constant.ContextKeyTokenId, 2)
		common.SetContextKey(c, constant.ContextKeyUserId, 3)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "g")
		common.SetContextKey(c, constant.ContextKeyIsStream, true)
		c.Set("token_name", "tk")
		c.Set("username", "u")
		c.Set(common.RequestIdKey, "req_mw_2")
		c.Set(common.UpstreamRequestIdKey, "up_req_mw_2")
		c.String(http.StatusOK,
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n"+
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"}}]}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":6}}\n\n"+
				"data: [DONE]\n")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "响应透传成功")

	var turn model.ConversationTurn
	require.Eventually(t, func() bool {
		return model.LOG_DB.Table("conversation_turns").Where("request_id = ?", "req_mw_2").First(&turn).Error == nil
	}, 3*time.Second, 10*time.Millisecond, "异步写库应在超时内完成")

	assert.True(t, turn.IsStream, "IsStream=true 经 ConversationInput 透传落库")
	assert.Equal(t, 12, turn.PromptTokens, "流式 usage prompt_tokens 落库")
	assert.Equal(t, 6, turn.CompletionTokens, "流式 usage completion_tokens 落库")

	var parts []service.MsgPart
	require.NoError(t, common.Unmarshal([]byte(turn.Messages), &parts), "Messages 应为合法 []MsgPart JSON")
	assert.Equal(t, []service.MsgPart{
		{Role: "user", Kind: "text", Text: "你好"},
		{Role: "assistant", Kind: "text", Text: "你好，世界"},
	}, parts, "请求侧+流式响应侧消息正确解析并落库")
}

// setupConversationLogTestDB 替换 model.DB/model.LOG_DB 为 sqlite 内存库并建
// conversation_turns 表，t.Cleanup 恢复原值。单连接保证异步写库 goroutine 与主测试
// goroutine 共享同一表。
func setupConversationLogTestDB(t *testing.T) {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	t.Cleanup(func() { model.DB, model.LOG_DB = previousDB, previousLogDB })
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
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
