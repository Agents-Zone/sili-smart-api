package middleware

import (
	"bytes"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// conversationBodyCaptureLimit 请求/响应体捕获上限（256KB，与 specs §3.2 响应 buffer
// 上限同阈值，请求 body 同阈值兜底）。响应侧经 conversationResponseWriter 截断并累计
// truncated；请求侧 GetBodyStorage 字节超限时按同一阈值截断，截断字节数并入 truncated。
// ConversationInput 的 Truncated 单一字段表达两侧截断总量，不新增请求侧字段、不改契约。
const conversationBodyCaptureLimit = 256 * 1024

// ConversationLog 返回挂载在 relay 路由分组上的 gin middleware。
// 读取 CONVERSATION_LOG_ENABLED：关时直接 c.Next() 零开销（不读 Body、不包装 writer）；
// 开时先按路径白名单过滤（见 isConversationPath），命中才继续捕获，随后
// gopool.Go 调 service.RecordConversation，闭包只捕获纯值。建表与 TTL 同步由
// migrateClickHouseLogDB 启动迁移负责（与 logs 同路径），middleware 只做捕获。
func ConversationLog() gin.HandlerFunc {
	enabled := common.GetEnvOrDefaultBool("CONVERSATION_LOG_ENABLED", false)
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

		// 请求体：GetBodyStorage 触发缓存，Bytes 取出后立即 Clone 并按 256KB 兜底截断。
		// memoryStorage.Bytes() 返回内部 slice 引用，BodyStorageCleanup 请求结束即 Close
		// storage。截断会让 RawRequestBody 成为非法 JSON，RecordConversation 的
		// parseRequestMessages 解析失败会记空请求侧消息（既有兜底路径，可接受）。
		var rawRequestBody []byte
		var requestTruncated int64
		if storage, err := common.GetBodyStorage(c); err == nil {
			if b, err := storage.Bytes(); err == nil {
				if len(b) > conversationBodyCaptureLimit {
					rawRequestBody = bytes.Clone(b[:conversationBodyCaptureLimit])
					requestTruncated = int64(len(b) - conversationBodyCaptureLimit)
				} else {
					rawRequestBody = bytes.Clone(b)
				}
			}
		}

		// 响应体：包装 c.Writer 捕获，上限 256KB。
		writer := &conversationResponseWriter{
			ResponseWriter: c.Writer,
			body:           bytes.NewBuffer(nil),
			maxSize:        conversationBodyCaptureLimit,
		}
		c.Writer = writer

		start := time.Now()
		c.Next()
		useTime := int64(time.Since(start).Seconds())

		// c.Next() 后的同步段捕获纯值标量，body 取出后立即 Clone。
		rawResponseBody := bytes.Clone(writer.body.Bytes())
		input := service.ConversationInput{
			RequestID:         c.GetString(common.RequestIdKey),
			CreatedAt:         common.GetTimestamp(),
			Truncated:         writer.truncated + requestTruncated,
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
// 捕获响应体到有限大小缓冲区。超限截断并累计 truncated 字节数，始终透传底层 writer，
// 避免大响应占用过多内存。
type conversationResponseWriter struct {
	gin.ResponseWriter
	body      *bytes.Buffer
	maxSize   int
	truncated int64 // 累计截断字节数
}

func (w *conversationResponseWriter) Write(b []byte) (int, error) {
	if w.body.Len() < w.maxSize {
		remain := w.maxSize - w.body.Len()
		if remain >= len(b) {
			w.body.Write(b)
		} else {
			w.body.Write(b[:remain])
			w.truncated += int64(len(b) - remain)
		}
	} else {
		w.truncated += int64(len(b))
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
