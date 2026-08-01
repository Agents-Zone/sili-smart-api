package model

import (
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

// ConversationTurn 一行一轮的会话轮次记录，仅落 ClickHouse 日志库（LOG_SQL_DSN）。
// Messages 存 []MsgPart 的 JSON 序列化（common.Marshal 写入），查询侧解码。
type ConversationTurn struct {
	Id                int64  `json:"id"`
	SessionKey        string `json:"session_key"`
	RequestId         string `json:"request_id"`
	CreatedAt         int64  `json:"created_at"`
	Messages          string `json:"messages"`
	TurnKind          string `json:"turn_kind"`
	Truncated         int64  `json:"truncated"`
	ModelName         string `json:"model_name"`
	ChannelId         int    `json:"channel_id"`
	TokenId           int    `json:"token_id"`
	TokenName         string `json:"token_name"`
	UserId            int    `json:"user_id"`
	Username          string `json:"username"`
	Group             string `json:"group"`
	Ip                string `json:"ip"`
	IsStream          bool   `json:"is_stream"`
	UseTime           int    `json:"use_time"`
	UpstreamRequestId string `json:"upstream_request_id"`
	PromptTokens      int    `json:"prompt_tokens"`
	CompletionTokens  int    `json:"completion_tokens"`
}

// ConversationSummary 会话列表聚合行，按 session_key 聚合，不含消息内容。
type ConversationSummary struct {
	SessionKey    string `json:"session_key"`
	FirstTurnTime int64  `json:"first_turn_time"`
	LastTurnTime  int64  `json:"last_turn_time"`
	TurnCount     int64  `json:"turn_count"`
	TokenName     string `json:"token_name"`
	Username      string `json:"username"`
	UserID        int64  `json:"user_id"`
	ModelName     string `json:"model_name"`
}

// ConversationQueryParams 会话列表过滤维度，对齐 logs 查询参数。
type ConversationQueryParams struct {
	TokenName      string
	Username       string
	ModelName      string
	StartTimestamp int64
	EndTimestamp   int64
}

var conversationEnsureOnce sync.Once

// EnsureConversationTable 幂等建表，sync.Once 保护。仅 ClickHouse 走手写 DDL，
// 非 ClickHouse 日志库不建表（conversation_turns 仅落 ClickHouse）。
func EnsureConversationTable() {
	conversationEnsureOnce.Do(func() {
		if common.LogDatabaseType() != common.DatabaseTypeClickHouse {
			return
		}
		if err := LOG_DB.Exec(conversationTurnCreateTableSQL(conversationTurnTTLDays())).Error; err != nil {
			common.SysError("failed to ensure conversation_turns table: " + err.Error())
		}
	})
}

// conversationTurnTTLDays 复用 LOG_SQL_CLICKHOUSE_TTL_DAYS，负值归 0（对齐 clickHouseLogTTLDays）。
func conversationTurnTTLDays() int {
	ttlDays := common.GetEnvOrDefault("LOG_SQL_CLICKHOUSE_TTL_DAYS", 0)
	if ttlDays < 0 {
		return 0
	}
	return ttlDays
}

// conversationTurnCreateTableSQL 生成 ClickHouse 建表 DDL。
// group 为保留字，列名用 logGroupCol 方言变量包裹；TTL 复用 clickHouseLogTTLClause。
func conversationTurnCreateTableSQL(ttlDays int) string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS conversation_turns (
	id Int64 DEFAULT 0,
	session_key String DEFAULT '',
	request_id String DEFAULT '',
	created_at Int64 DEFAULT 0,
	messages String DEFAULT '',
	turn_kind String DEFAULT 'normal',
	truncated Int64 DEFAULT 0,
	model_name String DEFAULT '',
	channel_id Int32 DEFAULT 0,
	token_id Int32 DEFAULT 0,
	token_name String DEFAULT '',
	user_id Int32 DEFAULT 0,
	username String DEFAULT '',
	%s String DEFAULT '',
	ip String DEFAULT '',
	is_stream UInt8 DEFAULT 0,
	use_time Int32 DEFAULT 0,
	upstream_request_id String DEFAULT '',
	prompt_tokens Int32 DEFAULT 0,
	completion_tokens Int32 DEFAULT 0
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(toDateTime(created_at))
ORDER BY (session_key, created_at, request_id)%s`, logGroupCol, clickHouseLogTTLClause(ttlDays))
}

// assignTurnIds 仿 assignDisplayLogIds，从 startIdx 起按序回填展示用 Id（ClickHouse 无自增）。
func assignTurnIds(turns []*ConversationTurn, startIdx int) {
	for i := range turns {
		turns[i].Id = int64(startIdx + i + 1)
	}
}

// RecordConversationTurn 写入一轮会话记录。写库失败经 common.SysError 记录，不阻塞调用方。
func RecordConversationTurn(turn *ConversationTurn) error {
	if turn == nil {
		return nil
	}
	if err := LOG_DB.Create(turn).Error; err != nil {
		common.SysError("failed to record conversation turn: " + err.Error())
		return err
	}
	return nil
}

// ListConversations 按 session_key 聚合分页，返回会话列表与总数。
// 维度列除 session_key 外均用 any() 聚合包裹（ClickHouse 跨库聚合约束），
// 首末轮时间用 min/max(created_at)，轮数用 count(*)；按末轮时间倒序。
func ListConversations(params *ConversationQueryParams, page, pageSize int) ([]ConversationSummary, int64, error) {
	var summaries []ConversationSummary

	subQuery := LOG_DB.Table("conversation_turns").Select("session_key")
	query := LOG_DB.Table("conversation_turns").Select("session_key, any(token_name) as token_name, any(username) as username, any(user_id) as user_id, any(model_name) as model_name, min(created_at) as first_turn_time, max(created_at) as last_turn_time, count(*) as turn_count")

	if params != nil {
		if params.TokenName != "" {
			subQuery = subQuery.Where("token_name = ?", params.TokenName)
			query = query.Where("token_name = ?", params.TokenName)
		}
		if params.Username != "" {
			subQuery = subQuery.Where("username = ?", params.Username)
			query = query.Where("username = ?", params.Username)
		}
		if params.ModelName != "" {
			subQuery = subQuery.Where("model_name = ?", params.ModelName)
			query = query.Where("model_name = ?", params.ModelName)
		}
		if params.StartTimestamp != 0 {
			subQuery = subQuery.Where("created_at >= ?", params.StartTimestamp)
			query = query.Where("created_at >= ?", params.StartTimestamp)
		}
		if params.EndTimestamp != 0 {
			subQuery = subQuery.Where("created_at <= ?", params.EndTimestamp)
			query = query.Where("created_at <= ?", params.EndTimestamp)
		}
	}

	// 总数：聚合会话数，用子查询包裹，避免直接 Count 在 GROUP BY 下产生分组计数。
	subQuery = subQuery.Group("session_key")
	var total int64
	if err := LOG_DB.Raw("SELECT count() FROM (?)", subQuery).Scan(&total).Error; err != nil {
		return nil, 0, err
	}

	if err := query.Group("session_key").Order("max(created_at) desc").Limit(pageSize).Offset((page - 1) * pageSize).Scan(&summaries).Error; err != nil {
		return nil, 0, err
	}
	return summaries, total, nil
}

// GetConversationTurns 按 created_at, request_id 升序取行。
// Messages 保持原始 JSON 字符串，解码在 service 层（model 不依赖 service 类型）。
func GetConversationTurns(sessionKey string) ([]ConversationTurn, error) {
	var turns []ConversationTurn
	err := LOG_DB.Table("conversation_turns").Where("session_key = ?", sessionKey).Order("created_at asc, request_id asc").Find(&turns).Error
	return turns, err
}
