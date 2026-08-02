package middleware

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// conversationLogIntegrationKey 集成接口读取密钥，启动时读一次（环境变量运行期不变）。
// CONVERSATION_LOG_ENABLED=true 时必须配置，否则 ValidateConversationLogAuth 阻止启动。
var conversationLogIntegrationKey = os.Getenv("CONVERSATION_LOG_INTEGRATION_KEY")

// ConversationLogAuth 集成密钥校验中间件，挂在 /api/conversation-log 路由组，供独立部署的
// AI 使用分析平台经 Bearer 密钥拉取对话原文，无需登录 new-api 后台。
// 密钥未配置（空串）直接 403，兜底未开采集却误挂路由的极端情况；不匹配返回 401。
// 用 Bearer 与项目其他对外接口（relay 的 sk-xxx、PAT）一致，分析平台侧标准 HTTP 库天然支持。
// 常量时间比较防时序攻击。
func ConversationLogAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if conversationLogIntegrationKey == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"success": false,
				"code":    "CONVERSATION_LOG_KEY_NOT_CONFIGURED",
				"message": "integration key not configured",
			})
			return
		}
		token := strings.TrimPrefix(c.Request.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(conversationLogIntegrationKey)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"code":    "CONVERSATION_LOG_INVALID_KEY",
				"message": "invalid integration key",
			})
			return
		}
		c.Next()
	}
}

// ValidateConversationLogAuth 启动校验：CONVERSATION_LOG_ENABLED=true 时必须配置
// CONVERSATION_LOG_INTEGRATION_KEY。在 main.go 的 model.InitLogDB() 之后调用
// （UsingLogDatabase 状态在那里面才 set，但本校验只依赖环境变量与密钥是否为空，不读库）。
// 校验失败经 common.FatalLog 终止进程，避免采集开启却无密钥保护的裸奔。
func ValidateConversationLogAuth() {
	if common.GetEnvOrDefaultBool("CONVERSATION_LOG_ENABLED", false) && conversationLogIntegrationKey == "" {
		common.FatalLog("CONVERSATION_LOG_ENABLED=true 但未配置 CONVERSATION_LOG_INTEGRATION_KEY，" +
			"conversation_turns 读取接口无密钥保护，拒绝启动")
	}
}
