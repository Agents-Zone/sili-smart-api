package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// conversationDetailMaxPageSize 长会话详情单响应轮数上限（对齐 specs §2.4 能力7：
// 单响应上限 200 轮，page_size 允许最大 200）。
const conversationDetailMaxPageSize = 200

// conversationTimeRangeInvalid 判断会话查询时间范围倒置：start_timestamp >
// end_timestamp 且均非 0 时视为参数非法（对齐 specs §2.4 能力7 的查询参数校验）。
func conversationTimeRangeInvalid(startTimestamp, endTimestamp int64) bool {
	return startTimestamp > endTimestamp && startTimestamp != 0 && endTimestamp != 0
}

// clampDetailPageSize 长会话详情页大小：超 200 clamp 到 200，非法值（<=0）回退
// 默认页大小（对齐 common.GetPageQuery 的 pageSize<=0 回退 ItemsPerPage 语义）。
func clampDetailPageSize(pageSize int) int {
	if pageSize <= 0 {
		return common.ItemsPerPage
	}
	if pageSize > conversationDetailMaxPageSize {
		return conversationDetailMaxPageSize
	}
	return pageSize
}

// ListConversations 返回会话列表，仅聚合字段（session_key、首末轮时间、轮数、
// token_name、username、user_id、model_name），不含消息内容。
// 查询参数与 logs 对齐：token_name/username/model_name/start_timestamp/end_timestamp；
// 分页沿用 common.GetPageQuery（page_size 上限 100）。
func ListConversations(c *gin.Context) {
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
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

// GetConversation 返回 {session, messages, turns}。session 为聚合元数据（token_name、
// username、user_id、model_name、首末轮时间、总轮数、是否还有后续轮次）；turns 为
// 逐轮元数据 {id, created_at, request_id, turn_kind, truncated}，不含每轮消息内容；
// messages 为按 created_at, request_id 升序 append 得到的完整 []MsgPart 序列，是
// 详情响应的唯一消息内容来源。长会话详情分页单独解析 p/page_size（不走 GetPageQuery
// 的 100 上限，允许最大 200），单响应上限 200 轮，超限截断并置 truncated 提示。
// 不回传整表维度字段（ip/channel_id/token_id/use_time 不进入响应）。
func GetConversation(c *gin.Context) {
	sessionKey := c.Param("session_key")
	if sessionKey == "" {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}

	page, _ := strconv.Atoi(c.Query("p"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	pageSize = clampDetailPageSize(pageSize)

	turns, err := model.GetConversationTurns(sessionKey)
	if err != nil {
		common.ApiErrorI18n(c, i18n.MsgDatabaseError)
		return
	}
	if len(turns) == 0 {
		common.ApiErrorI18n(c, i18n.MsgConversationNotFound)
		return
	}

	startIdx := (page - 1) * pageSize
	endIdx := startIdx + pageSize
	if startIdx >= len(turns) {
		startIdx = len(turns)
	}
	if endIdx > len(turns) {
		endIdx = len(turns)
	}
	pageTurns := turns[startIdx:endIdx]

	// session 聚合元数据取首行维度字段 + 首末轮时间 + 总轮数；truncated 表示
	// 当前页之后仍有轮次未返回。
	session := gin.H{
		"token_name":      turns[0].TokenName,
		"username":        turns[0].Username,
		"user_id":         turns[0].UserId,
		"model_name":      turns[0].ModelName,
		"first_turn_time": turns[0].CreatedAt,
		"last_turn_time":  turns[len(turns)-1].CreatedAt,
		"turn_count":      len(turns),
		"truncated":       endIdx < len(turns),
	}

	// turns 为逐轮元数据，显式只取五列，避免整表维度字段进入响应。
	turnMetas := make([]gin.H, 0, len(pageTurns))
	for _, turn := range pageTurns {
		turnMetas = append(turnMetas, gin.H{
			"id":         turn.Id,
			"created_at": turn.CreatedAt,
			"request_id": turn.RequestId,
			"turn_kind":  turn.TurnKind,
			"truncated":  turn.Truncated,
		})
	}

	messages := service.MergeConversation(pageTurns)
	common.ApiSuccess(c, gin.H{
		"session":  session,
		"turns":    turnMetas,
		"messages": messages,
	})
}
