package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestConversationTurnCreateTableSQL(t *testing.T) {
	original := common.LogDatabaseType()
	t.Cleanup(func() {
		common.SetLogDatabaseType(original)
		initCol()
	})
	common.SetLogDatabaseType(common.DatabaseTypeClickHouse)
	initCol()

	withoutTTL := conversationTurnCreateTableSQL(0)
	assert.Contains(t, withoutTTL, "CREATE TABLE IF NOT EXISTS conversation_turns")
	assert.Contains(t, withoutTTL, "ENGINE = MergeTree()")
	assert.Contains(t, withoutTTL, "PARTITION BY toYYYYMM(toDateTime(created_at))")
	assert.Contains(t, withoutTTL, "ORDER BY (session_key, created_at, request_id)")
	assert.Contains(t, withoutTTL, "`group` String DEFAULT ''")
	assert.NotContains(t, withoutTTL, "TTL ")

	withTTL := conversationTurnCreateTableSQL(30)
	assert.Contains(t, withTTL, "TTL toDateTime(created_at) + INTERVAL 30 DAY DELETE")
	assert.Contains(t, withTTL, "ORDER BY (session_key, created_at, request_id)")

	negativeTTL := conversationTurnCreateTableSQL(-5)
	assert.NotContains(t, negativeTTL, "TTL ")
}

func TestAssignTurnIds(t *testing.T) {
	turns := []*ConversationTurn{{}, {}, {}}

	assignTurnIds(turns, 0)
	assert.Equal(t, []int64{1, 2, 3}, []int64{turns[0].Id, turns[1].Id, turns[2].Id})

	assignTurnIds(turns, 20)
	assert.Equal(t, []int64{21, 22, 23}, []int64{turns[0].Id, turns[1].Id, turns[2].Id})

	assert.NotPanics(t, func() { assignTurnIds(nil, 0) })
	assert.NotPanics(t, func() { assignTurnIds([]*ConversationTurn{}, 0) })
}

func TestConversationTurnTTLDays(t *testing.T) {
	t.Setenv("LOG_SQL_CLICKHOUSE_TTL_DAYS", "30")
	assert.Equal(t, 30, conversationTurnTTLDays())

	t.Setenv("LOG_SQL_CLICKHOUSE_TTL_DAYS", "-5")
	assert.Equal(t, 0, conversationTurnTTLDays())
}

func setupConversationTurnTestDB(t *testing.T) {
	t.Helper()
	previousDB, previousLogDB := DB, LOG_DB
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLogDB
	})
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	DB, LOG_DB = db, db
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS conversation_turns (
		id INTEGER DEFAULT 0,
		session_key TEXT DEFAULT '',
		request_id TEXT DEFAULT '',
		created_at INTEGER DEFAULT 0,
		messages TEXT DEFAULT '',
		turn_kind TEXT DEFAULT 'normal',
		truncated INTEGER DEFAULT 0,
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

func TestEnsureConversationTableNonClickHouse(t *testing.T) {
	original := common.LogDatabaseType()
	t.Cleanup(func() { common.SetLogDatabaseType(original) })
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)
	// 非 ClickHouse 日志库下不建表、不访问 LOG_DB（conversation_turns 仅落 ClickHouse）
	assert.NotPanics(t, EnsureConversationTable)
}

func TestRecordConversationTurn(t *testing.T) {
	setupConversationTurnTestDB(t)
	turn := &ConversationTurn{
		SessionKey:        "conv_test",
		RequestId:         "req_1",
		CreatedAt:         1781234567,
		Messages:          `[{"role":"user","kind":"text","text":"hi"}]`,
		TurnKind:          "first",
		ModelName:         "gpt-4o",
		ChannelId:         1,
		TokenId:           2,
		TokenName:         "tk",
		UserId:            3,
		Username:          "u",
		Group:             "g",
		Ip:                "1.2.3.4",
		IsStream:          true,
		UseTime:           1,
		UpstreamRequestId: "up_req",
		PromptTokens:      10,
		CompletionTokens:  20,
	}
	require.NoError(t, RecordConversationTurn(turn))

	var got ConversationTurn
	require.NoError(t, LOG_DB.Table("conversation_turns").Where("session_key = ?", "conv_test").First(&got).Error)
	assert.Equal(t, turn.RequestId, got.RequestId)
	assert.Equal(t, turn.Messages, got.Messages)
	assert.Equal(t, turn.TurnKind, got.TurnKind)
	assert.Equal(t, turn.Group, got.Group)
	assert.Equal(t, turn.Username, got.Username)
	assert.Equal(t, turn.PromptTokens, got.PromptTokens)
	assert.Equal(t, turn.CompletionTokens, got.CompletionTokens)
}

func TestGetConversationTurnsOrder(t *testing.T) {
	setupConversationTurnTestDB(t)
	for _, turn := range []*ConversationTurn{
		{SessionKey: "s", RequestId: "b", CreatedAt: 10, Messages: "m2", TurnKind: "normal"},
		{SessionKey: "s", RequestId: "a", CreatedAt: 10, Messages: "m1", TurnKind: "first"},
		{SessionKey: "s", RequestId: "c", CreatedAt: 5, Messages: "m0", TurnKind: "first"},
	} {
		require.NoError(t, RecordConversationTurn(turn))
	}

	got, err := GetConversationTurns("s")
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "m0", got[0].Messages)
	assert.Equal(t, "m1", got[1].Messages)
	assert.Equal(t, "m2", got[2].Messages)

	empty, err := GetConversationTurns("missing")
	require.NoError(t, err)
	assert.Len(t, empty, 0)
}

func TestRecordConversationTurnZeroValueDimensions(t *testing.T) {
	setupConversationTurnTestDB(t)
	// 失败/错误响应入库时缺失维度记零值，model 层须接受零值字段正常写入
	turn := &ConversationTurn{
		SessionKey: "conv_zero",
		RequestId:  "req_zero",
		CreatedAt:  100,
	}
	require.NoError(t, RecordConversationTurn(turn))

	var got ConversationTurn
	require.NoError(t, LOG_DB.Table("conversation_turns").Where("session_key = ?", "conv_zero").First(&got).Error)
	assert.Equal(t, int64(0), got.Truncated)
	assert.Equal(t, "", got.ModelName)
	assert.Equal(t, "", got.TokenName)
	assert.Equal(t, 0, got.ChannelId)
	assert.Equal(t, 0, got.PromptTokens)
	assert.Equal(t, 0, got.CompletionTokens)
}
