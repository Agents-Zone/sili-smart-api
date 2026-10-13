package controller

// 全链路回归测试：多密钥渠道 + 余额不足错误 + 自定义关键词命中
// → processChannelError → DisableChannel(gopool 异步) → UpdateChannelStatus，
// 断言出错的 key 真正被禁用且渠道整体保持启用（还有其他可用 key）。

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupRelayErrorDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}, &model.Log{}))
	// :memory: 每个连接是独立库，必须限制单连接，否则异步 goroutine 的查询会看不到表
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})
}

func TestProcessChannelErrorDisablesMultiKeyChannelKey(t *testing.T) {
	setupRelayErrorDB(t)

	// 开启自动禁用 + 配置中文关键词
	origDisable := common.AutomaticDisableChannelEnabled
	origKeywords := operation_setting.AutomaticDisableKeywords
	origErrorLog := constant.ErrorLogEnabled
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableKeywordsFromString("余额不足")
	constant.ErrorLogEnabled = false
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = origDisable
		operation_setting.AutomaticDisableKeywords = origKeywords
		constant.ErrorLogEnabled = origErrorLog
	})

	autoBan := 1
	ch := &model.Channel{
		Id:      1,
		Name:    "mk",
		Key:     "sk-a\nsk-b",
		Status:  common.ChannelStatusEnabled,
		Models:  "gpt-test",
		Group:   "default",
		AutoBan: &autoBan,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeyMode: "polling",
		},
	}
	require.NoError(t, model.DB.Create(ch).Error)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// 模拟 relay 失败现场：请求用的 key 是 sk-a
	common.SetContextKey(c, constant.ContextKeyChannelKey, "sk-a")

	balanceErr := types.NewErrorWithStatusCode(
		fmt.Errorf(`{"error":{"message":"账户余额不足，请充值","type":"quota_error"}}`),
		types.ErrorCodeBadResponse, 402)

	channelError := *types.NewChannelError(ch.Id, ch.Type, ch.Name, ch.ChannelInfo.IsMultiKey, "sk-a", ch.GetAutoBan())
	processChannelError(c, channelError, balanceErr)

	// DisableChannel 在 gopool 异步执行，条件等待 DB 变化
	require.Eventually(t, func() bool {
		after, err := model.GetChannelById(ch.Id, true)
		if err != nil {
			return false
		}
		return after.ChannelInfo.MultiKeyStatusList != nil &&
			after.ChannelInfo.MultiKeyStatusList[0] == common.ChannelStatusAutoDisabled
	}, 3*time.Second, 50*time.Millisecond, "关键词命中后，出错的 key 必须被自动禁用")

	after, err := model.GetChannelById(ch.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, after.Status, "还剩 sk-b，渠道整体应保持启用")
	assert.NotContains(t, after.ChannelInfo.MultiKeyStatusList, 1, "sk-b 不应被禁用")
}
