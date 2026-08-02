package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// conversationTimeRangeInvalid 判断会话查询时间范围倒置：start_timestamp >
// end_timestamp 且均非 0 时视为参数非法（对齐 specs §2.4 能力7 的查询参数校验）。
func conversationTimeRangeInvalid(startTimestamp, endTimestamp int64) bool {
	return startTimestamp > endTimestamp && startTimestamp != 0 && endTimestamp != 0
}

// ListConversations 返回会话列表，仅聚合字段（session_key、首末轮时间、轮数、
// token_name、username、user_id、model_name），不含消息内容。
// 查询参数与 logs 对齐：token_name/username/model_name/start_timestamp/end_timestamp；
// 分页沿用 common.GetPageQuery（page_size 上限 100）。
func ListConversations(c *gin.Context) {
	// 时间戳参数可选：缺失（空串）按 0 处理即不设过滤；非数字视为参数非法返回 400
	// （对齐 specs §2.4 能力7 查询参数校验，避免管理员拼写错误拿到未过滤的宽数据）。
	startTimestampStr := c.Query("start_timestamp")
	startTimestamp, err := strconv.ParseInt(startTimestampStr, 10, 64)
	if err != nil && startTimestampStr != "" {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	endTimestampStr := c.Query("end_timestamp")
	endTimestamp, err := strconv.ParseInt(endTimestampStr, 10, 64)
	if err != nil && endTimestampStr != "" {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	if conversationTimeRangeInvalid(startTimestamp, endTimestamp) {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}

	params := &model.ConversationQueryParams{
		TokenName:      c.Query("token_name"),
		Username:       c.Query("username"),
		ModelName:      c.Query("model_name"),
		StartTimestamp: startTimestamp,
		EndTimestamp:   endTimestamp,
	}
	pageInfo := common.GetPageQuery(c)
	summaries, total, err := model.ListConversations(params, pageInfo.GetPage(), pageInfo.GetPageSize())
	if err != nil {
		common.ApiErrorI18n(c, i18n.MsgDatabaseError)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(summaries)
	common.ApiSuccess(c, pageInfo)
}

// GetConversation 返回 {session, messages, turns}。session 为聚合元数据（session_key、
// token_name、username、user_id、model_name、首末轮时间、总轮数）；turns 为逐轮元数据
// {id, created_at, request_id, turn_kind}，不含每轮消息内容；messages 为按 created_at,
// request_id 升序 append 全量 turns 得到的完整 []MsgPart 序列，是详情响应的唯一消息内容
// 来源。增量存储下单 session 完整对话为 O(N)（每条消息存一次），messages 与 turns 元数据
// 均全量返回、不分页（turns 仅 id/created_at/request_id/turn_kind 四个轻量字段）。
// 不回传整表维度字段（ip/channel_id/token_id/use_time 不进入响应）。
func GetConversation(c *gin.Context) {
	sessionKey := c.Param("session_key")
	if sessionKey == "" {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}

	turns, err := model.GetConversationTurns(sessionKey)
	if err != nil {
		common.ApiErrorI18n(c, i18n.MsgDatabaseError)
		return
	}
	if len(turns) == 0 {
		common.ApiErrorI18n(c, i18n.MsgConversationNotFound)
		return
	}

	// session 聚合元数据取首行维度字段 + session_key + 首末轮时间 + 总轮数。
	session := gin.H{
		"token_name":      turns[0].TokenName,
		"username":        turns[0].Username,
		"user_id":         turns[0].UserId,
		"model_name":      turns[0].ModelName,
		"session_key":     sessionKey,
		"first_turn_time": turns[0].CreatedAt,
		"last_turn_time":  turns[len(turns)-1].CreatedAt,
		"turn_count":      len(turns),
	}

	// turns 为逐轮元数据，显式只取四列，避免整表维度字段进入响应。
	turnMetas := make([]gin.H, 0, len(turns))
	for _, turn := range turns {
		turnMetas = append(turnMetas, gin.H{
			"id":         turn.Id,
			"created_at": turn.CreatedAt,
			"request_id": turn.RequestId,
			"turn_kind":  turn.TurnKind,
		})
	}

	messages := service.MergeConversation(turns)
	common.ApiSuccess(c, gin.H{
		"session":  session,
		"turns":    turnMetas,
		"messages": messages,
	})
}
