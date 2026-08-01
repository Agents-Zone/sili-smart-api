package middleware

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// ConversationLog 返回挂载在 relay 路由分组上的 gin middleware。
// 读取 CONVERSATION_LOG_ENABLED：关时直接 c.Next() 零开销（不读 Body、不包装 writer）；
// 开时先按路径白名单过滤（见 isConversationPath），命中才继续捕获，随后
// gopool.Go 调 service.RecordConversation，闭包只捕获纯值。建表与 TTL 同步由
// migrateClickHouseLogDB 启动迁移负责（与 logs 同路径），middleware 只做捕获。
func ConversationLog() gin.HandlerFunc {
	enabled := common.GetEnvOrDefaultBool("CONVERSATION_LOG_ENABLED", false)
	// 安全阀：请求/响应体捕获上限（KB）。默认 0 = 无限（全量捕获，不截断）；设为正值时，
	// 超出部分丢弃并经 SysError 记录丢弃量，兜底异常超大请求/响应的内存峰值（OOM 安全阀）。
	bodyLimit := common.GetEnvOrDefault("CONVERSATION_LOG_BODY_LIMIT_KB", 0) * 1024
	return func(c *gin.Context) {
		if !enabled {
			c.Next()
			return
		}
		// 路径白名单过滤，未命中直接透传。
		if !isConversationPath(c.Request.URL.Path) {
			c.Next()
			return
		}

		// 请求体：GetBodyStorage 触发缓存，Bytes 取出后立即 Clone（默认全量，不截断）。
		// memoryStorage.Bytes() 返回内部 slice 引用，BodyStorageCleanup 请求结束即 Close
		// storage。仅当安全阀 bodyLimit > 0 且超限时按上限截断克隆，丢弃量计入 requestDropped，
		// 经下方 SysError 记录；默认 bodyLimit=0 走全量分支，请求体不再被截断为非法 JSON。
		var rawRequestBody []byte
		var requestDropped int64
		if storage, err := common.GetBodyStorage(c); err == nil {
			if b, err := storage.Bytes(); err == nil {
				if bodyLimit > 0 && len(b) > bodyLimit {
					rawRequestBody = bytes.Clone(b[:bodyLimit])
					requestDropped = int64(len(b) - bodyLimit)
				} else {
					rawRequestBody = bytes.Clone(b)
				}
			}
		}

		// 响应体：包装 c.Writer 捕获，maxSize=0（默认）全量缓存；>0 时超限丢弃并累计 droppedBytes。
		writer := &conversationResponseWriter{
			ResponseWriter: c.Writer,
			body:           bytes.NewBuffer(nil),
			maxSize:        bodyLimit,
		}
		c.Writer = writer

		start := time.Now()
		c.Next()
		useTime := int64(time.Since(start).Seconds())

		// 安全阀触发时记录丢弃量：同步段上下文完整（request_id/path 可读），日志写入安全。
		// 默认 bodyLimit=0 时 requestDropped 与 writer.droppedBytes 恒 0，不产生日志噪声。
		if requestDropped > 0 || writer.droppedBytes > 0 {
			common.SysError(fmt.Sprintf(
				"conversation log: body dropped beyond CONVERSATION_LOG_BODY_LIMIT_KB safety valve "+
					"(limit=%d bytes, request_dropped=%d, response_dropped=%d, request_id=%s, path=%s)",
				bodyLimit, requestDropped, writer.droppedBytes,
				c.GetString(common.RequestIdKey), c.Request.URL.Path))
		}

		// c.Next() 后的同步段捕获纯值标量，body 取出后立即 Clone。
		rawResponseBody := bytes.Clone(writer.body.Bytes())
		input := service.ConversationInput{
			RequestID:         c.GetString(common.RequestIdKey),
			CreatedAt:         common.GetTimestamp(),
			ModelName:         common.GetContextKeyString(c, constant.ContextKeyOriginalModel),
			ChannelID:         common.GetContextKeyInt(c, constant.ContextKeyChannelId),
			TokenID:           common.GetContextKeyInt(c, constant.ContextKeyTokenId),
			TokenName:         c.GetString("token_name"),
			UserID:            common.GetContextKeyInt(c, constant.ContextKeyUserId),
			Username:          c.GetString("username"),
			Group:             common.GetContextKeyString(c, constant.ContextKeyUsingGroup),
			IP:                c.ClientIP(),
			UseTime:           useTime,
			IsStream:          common.GetContextKeyBool(c, constant.ContextKeyIsStream),
			UpstreamRequestID: c.GetString(common.UpstreamRequestIdKey),
			Path:              c.Request.URL.Path,
			RawRequestBody:    rawRequestBody,
			RawResponseBody:   rawResponseBody,
		}
		// 异步写库，闭包只捕获 input 纯值快照，不阻塞响应。
		gopool.Go(func() {
			service.RecordConversation(input)
		})
	}
}

// conversationResponseWriter 照抄 auditResponseWriter 的包装模式：嵌入 gin.ResponseWriter，
// 捕获响应体到缓冲区。maxSize=0（默认）全量缓存不截断；maxSize>0 时超限丢弃并累计
// droppedBytes（仅经 SysError 记录，不再有字段承载），始终透传底层 writer。
type conversationResponseWriter struct {
	gin.ResponseWriter
	body         *bytes.Buffer
	maxSize      int   // 0 = 无限（默认全量捕获）；>0 = 超出后丢弃
	droppedBytes int64 // 仅 maxSize>0 且超限时累计，默认 0
}

func (w *conversationResponseWriter) Write(b []byte) (int, error) {
	if w.maxSize <= 0 {
		w.body.Write(b)
	} else if w.body.Len() < w.maxSize {
		remain := w.maxSize - w.body.Len()
		if remain >= len(b) {
			w.body.Write(b)
		} else {
			w.body.Write(b[:remain])
			w.droppedBytes += int64(len(b) - remain)
		}
	} else {
		w.droppedBytes += int64(len(b))
	}
	return w.ResponseWriter.Write(b)
}

func (w *conversationResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// isConversationPath 路径白名单，集中在此一处：OpenAI/Claude 精确匹配，
// Gemini 后缀匹配（覆盖 /v1beta 与 /v1 两个入口）。不覆盖 /v1/realtime、
// 转写/TTS/embedding/image/rerank 等非对话路径。
func isConversationPath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/responses/compact", "/v1/messages":
		return true
	}
	return strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")
}
