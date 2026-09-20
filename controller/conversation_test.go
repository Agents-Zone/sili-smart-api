package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupConversationControllerTestDB 初始化 controller 会话查询测试夹具：
// 内存 SQLite 作为主库与日志库（LOG_DB），AutoMigrate conversation_turns 表，
// 并初始化 i18n 使 common.ApiErrorI18n 输出翻译后的消息。
func setupConversationControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	// 快照包级全局并在 cleanup 恢复，消除包内测试顺序依赖（沿用
	// user_manage_test.go setupManageUserTestDB 的快照恢复模式）。
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	previousMainDatabaseType, previousLogDatabaseType := common.MainDatabaseType(), common.LogDatabaseType()

	gin.SetMode(gin.TestMode)
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	require.NoError(t, i18n.Init())

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(&model.ConversationTurn{}))

	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled = previousRedisEnabled
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// insertConversationTurns 写入 turnCount 轮同一会话的轮次记录，created_at 自 1000 起递增，
// 每轮 messages 为单个 user/text 片段。维度字段填值以验证不回传。
func insertConversationTurns(t *testing.T, db *gorm.DB, sessionKey string, turnCount int) {
	t.Helper()
	for i := 0; i < turnCount; i++ {
		messages, err := common.Marshal([]service.MsgPart{
			{Role: "user", Kind: "text", Text: fmt.Sprintf("msg-%d", i)},
		})
		require.NoError(t, err)
		turn := &model.ConversationTurn{
			Id:         int64(i + 1),
			SessionKey: sessionKey,
			RequestId:  fmt.Sprintf("req-%d", i),
			CreatedAt:  int64(1000 + i),
			Messages:   string(messages),
			TurnKind:   "normal",
			TokenName:  "test-token",
			Username:   "test-user",
			UserId:     7,
			ModelName:  "gpt-4o",
			Ip:         "1.2.3.4",
			ChannelId:  99,
			TokenId:    88,
			UseTime:    123,
		}
		require.NoError(t, db.Create(turn).Error)
	}
}

// conversationErrorPayload 解析错误响应的 success/message。
type conversationErrorPayload struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// conversationDetailPayload 解析详情响应的 data 结构。
type conversationDetailPayload struct {
	Success bool `json:"success"`
	Data    struct {
		Session  map[string]any    `json:"session"`
		Turns    []map[string]any  `json:"turns"`
		Messages []service.MsgPart `json:"messages"`
	} `json:"data"`
}

func TestConversationTimeRangeInvalid(t *testing.T) {
	require.False(t, conversationTimeRangeInvalid(0, 0))
	require.False(t, conversationTimeRangeInvalid(0, 50))
	require.False(t, conversationTimeRangeInvalid(100, 0))
	require.False(t, conversationTimeRangeInvalid(50, 50))
	require.False(t, conversationTimeRangeInvalid(50, 100))
	require.True(t, conversationTimeRangeInvalid(100, 50))
}

func TestListConversationsRejectsInvertedTimeRange(t *testing.T) {
	setupConversationControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/conversation/?start_timestamp=100&end_timestamp=50", nil)

	ListConversations(ctx)

	var payload conversationErrorPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.False(t, payload.Success)
	require.Equal(t, "Invalid parameters", payload.Message)
}

func TestGetConversationMissingSessionKey(t *testing.T) {
	setupConversationControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/conversation/", nil)

	GetConversation(ctx)

	var payload conversationErrorPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.False(t, payload.Success)
	require.Equal(t, "Invalid parameters", payload.Message)
}

func TestGetConversationNotFound(t *testing.T) {
	setupConversationControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "session_key", Value: "no-such-session"}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/conversation/no-such-session", nil)

	GetConversation(ctx)

	var payload conversationErrorPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.False(t, payload.Success)
	require.Equal(t, "conversation not found", payload.Message)
}

func TestGetConversationReturnsSessionTurnsAndMergedMessages(t *testing.T) {
	db := setupConversationControllerTestDB(t)
	insertConversationTurns(t, db, "conv-test-1", 3)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "session_key", Value: "conv-test-1"}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/conversation/conv-test-1", nil)

	GetConversation(ctx)

	var payload conversationDetailPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)

	session := payload.Data.Session
	require.Equal(t, "test-token", session["token_name"])
	require.Equal(t, "test-user", session["username"])
	require.Equal(t, float64(7), session["user_id"])
	require.Equal(t, "gpt-4o", session["model_name"])
	require.Equal(t, "conv-test-1", session["session_key"])
	require.Equal(t, float64(1000), session["first_turn_time"])
	require.Equal(t, float64(1002), session["last_turn_time"])
	require.Equal(t, float64(3), session["turn_count"])
	require.NotContains(t, session, "truncated")
	require.NotContains(t, session, "ip")
	require.NotContains(t, session, "channel_id")
	require.NotContains(t, session, "token_id")
	require.NotContains(t, session, "use_time")

	require.Len(t, payload.Data.Turns, 3)
	for _, turn := range payload.Data.Turns {
		require.Contains(t, turn, "id")
		require.Contains(t, turn, "created_at")
		require.Contains(t, turn, "request_id")
		require.Contains(t, turn, "turn_kind")
		require.NotContains(t, turn, "truncated")
		require.NotContains(t, turn, "messages")
		require.NotContains(t, turn, "ip")
		require.NotContains(t, turn, "channel_id")
		require.NotContains(t, turn, "token_id")
		require.NotContains(t, turn, "use_time")
	}

	require.Len(t, payload.Data.Messages, 3)
	require.Equal(t, "msg-0", payload.Data.Messages[0].Text)
	require.Equal(t, "msg-1", payload.Data.Messages[1].Text)
	require.Equal(t, "msg-2", payload.Data.Messages[2].Text)
}

// TestGetConversationReturnsAllTurnsNoPaging 详情不再分页：长会话（250 轮）一次性全量
// 返回 turns 与 messages，session 无 truncated 字段；page_size 查询参数被忽略。
func TestGetConversationReturnsAllTurnsNoPaging(t *testing.T) {
	db := setupConversationControllerTestDB(t)
	insertConversationTurns(t, db, "conv-full", 250)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "session_key", Value: "conv-full"}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/conversation/conv-full?page_size=50", nil)

	GetConversation(ctx)

	var payload conversationDetailPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	require.Len(t, payload.Data.Turns, 250, "详情不再分页，全量返回 250 轮")
	require.Len(t, payload.Data.Messages, 250, "messages 全量拼接 250 段")
	require.NotContains(t, payload.Data.Session, "truncated", "session 不再有 truncated 字段")
	require.Equal(t, float64(250), payload.Data.Session["turn_count"])
}

func TestListConversationsRejectsNonNumericTimestamp(t *testing.T) {
	setupConversationControllerTestDB(t)

	for _, query := range []string{
		"/api/conversation/?start_timestamp=abc",
		"/api/conversation/?end_timestamp=abc",
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, query, nil)

		ListConversations(ctx)

		var payload conversationErrorPayload
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
		require.False(t, payload.Success)
		require.Equal(t, "Invalid parameters", payload.Message)
	}
}

func TestListConversationsAbsentTimestampDoesNotReject(t *testing.T) {
	setupConversationControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/conversation/", nil)

	ListConversations(ctx)

	// 缺席时间戳须按「不设过滤」处理，不能在解析阶段被 400 拒绝；后续 DB 查询
	// 依赖 ClickHouse 的 any() 聚合，SQLite 夹具无法跑通，故只断言未返回参数非法。
	var payload conversationErrorPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.NotEqual(t, "Invalid parameters", payload.Message)
}
