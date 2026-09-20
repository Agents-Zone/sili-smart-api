package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 通过真实分发中间件验证 token_id 绑定和普通/任务中转的重试入口。
func TestRelayExclusiveAffinityLifecycle(t *testing.T) {
	require.NoError(t, i18n.Init())
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	oldDB, oldMemory, oldRedis := model.DB, common.MemoryCacheEnabled, common.RedisEnabled
	setting := operation_setting.GetChannelAffinitySetting()
	oldSetting := *setting
	model.DB, common.MemoryCacheEnabled, common.RedisEnabled = db, true, false
	*setting = operation_setting.ChannelAffinitySetting{
		Enabled: true, SwitchOnSuccess: true, KeepOnChannelDisabled: true, DefaultTTLSeconds: 60,
		Rules: []operation_setting.ChannelAffinityRule{{Name: t.Name(), IncludeRuleName: true, ModelRegex: []string{"^gpt-"},
			ExclusiveBind: true, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}}}},
	}
	t.Cleanup(func() {
		service.ClearChannelAffinityRuntimeByChannelIDs([]int{9801, 9802})
		*setting = oldSetting
		model.DB, common.MemoryCacheEnabled, common.RedisEnabled = oldDB, oldMemory, oldRedis
		if oldMemory && oldDB != nil {
			model.InitChannelCache()
		}
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	for _, id := range []int{9801, 9802} {
		require.NoError(t, db.Create(&model.Channel{Id: id, Type: constant.ChannelTypeOpenAI,
			Status: common.ChannelStatusEnabled, Key: "test", Models: "gpt-4,gpt-5", Group: "default"}).Error)
		for _, name := range []string{"gpt-4", "gpt-5"} {
			require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: "default", Model: name, Enabled: true}).Error)
		}
	}
	model.InitChannelCache()
	status, selected, calls := http.StatusOK, 0, 0
	engine := gin.New()
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set("token_id", 123456)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	}, middleware.Distribute(), func(c *gin.Context) {
		calls++
		selected = c.GetInt("channel_id")
		if status >= 400 {
			err := types.NewErrorWithStatusCode(errors.New("测试上游失败"), types.ErrorCodeBadResponseStatusCode, status)
			assert.False(t, shouldRetry(c, err, 3))
			assert.False(t, shouldRetryTaskRelay(c, selected, &taskdto.TaskError{StatusCode: status}, 3))
			service.RollbackChannelAffinityOnFinalFailure(c)
		}
		c.Status(status)
	})
	request := func(name string) int {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+name+`"}`))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(recorder, req)
		return recorder.Code
	}
	require.Equal(t, 200, request("gpt-4"))
	first := selected
	for _, code := range []int{429, 500, 502, 400, 402, 200} {
		status = code
		assert.Equal(t, code, request("gpt-4"))
		assert.Equal(t, first, selected)
	}
	// 渠道仍启用时，模型不可用应保留绑定并返回错误。
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ? AND model = ?", first, "gpt-5").Update("enabled", false).Error)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", first).Update("models", "gpt-4").Error)
	model.InitChannelCache()
	before := calls
	assert.Equal(t, 503, request("gpt-5"))
	assert.Equal(t, before, calls)
	assert.Equal(t, 200, request("gpt-4"))
	assert.Equal(t, first, selected)
	for _, disabledStatus := range []int{common.ChannelStatusManuallyDisabled, common.ChannelStatusAutoDisabled} {
		previous := selected
		require.True(t, model.UpdateChannelStatus(previous, "", disabledStatus, "测试禁用"))
		require.Equal(t, 200, request("gpt-4"))
		assert.NotEqual(t, previous, selected)
		require.True(t, model.UpdateChannelStatus(previous, "", common.ChannelStatusEnabled, "测试恢复"))
		model.InitChannelCache()
		rebound := selected
		assert.Equal(t, 200, request("gpt-4"))
		assert.Equal(t, rebound, selected)
	}
}
