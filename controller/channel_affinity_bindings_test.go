package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetChannelAffinityBindingsHandlerSuccess(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Channel{}, &model.Token{}))
	oldDB, oldLogDB, oldMemory, oldRedis := model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled
	oldSetting := *operation_setting.GetChannelAffinitySetting()
	model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled = db, db, true, false
	*operation_setting.GetChannelAffinitySetting() = operation_setting.ChannelAffinitySetting{Enabled: true, DefaultTTLSeconds: 60,
		Rules: []operation_setting.ChannelAffinityRule{{Name: "test", IncludeRuleName: true, ModelRegex: []string{"^gpt-"}, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}}}}}
	require.NoError(t, db.Create(&model.Channel{Id: 1, Name: "主渠道"}).Error)
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "u", Role: 1}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 101, UserId: 1, Name: "生产令牌"}).Error)
	service.ClearChannelAffinityCacheAll()
	t.Cleanup(func() {
		service.ClearChannelAffinityCacheAll()
		*operation_setting.GetChannelAffinitySetting() = oldSetting
		model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled = oldDB, oldLogDB, oldMemory, oldRedis
		if sqlDB, closeErr := db.DB(); closeErr == nil {
			_ = sqlDB.Close()
		}
	})

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ctx.Set("token_id", 101)
	_, found := service.GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
	require.False(t, found)
	service.RecordChannelAffinity(ctx, 1)

	w := httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/option/channel_affinity_bindings", nil)
	GetChannelAffinityBindings(ctx)
	require.Equal(t, http.StatusOK, w.Code)
	var payload struct {
		Success bool                             `json:"success"`
		Message string                           `json:"message"`
		Data    []service.ChannelAffinityBinding `json:"data"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	require.Empty(t, payload.Message)
	require.Len(t, payload.Data, 1)
	require.Equal(t, 1, payload.Data[0].ChannelID)
	require.Len(t, payload.Data[0].Tokens, 1)
	require.Equal(t, 101, payload.Data[0].Tokens[0].TokenID)
}

func TestGetChannelAffinityBindingsHandlerEmpty(t *testing.T) {
	oldSetting := *operation_setting.GetChannelAffinitySetting()
	*operation_setting.GetChannelAffinitySetting() = operation_setting.ChannelAffinitySetting{}
	t.Cleanup(func() { *operation_setting.GetChannelAffinitySetting() = oldSetting })
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/option/channel_affinity_bindings", nil)
	GetChannelAffinityBindings(c)
	var payload struct {
		Success bool                             `json:"success"`
		Data    []service.ChannelAffinityBinding `json:"data"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	require.NotNil(t, payload.Data)
	require.Len(t, payload.Data, 0)
}

func TestGetChannelAffinityBindingsHandlerFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	oldDB, oldMemory := model.DB, common.MemoryCacheEnabled
	oldSetting := *operation_setting.GetChannelAffinitySetting()
	model.DB, common.MemoryCacheEnabled = db, true
	*operation_setting.GetChannelAffinitySetting() = operation_setting.ChannelAffinitySetting{Enabled: true, DefaultTTLSeconds: 60,
		Rules: []operation_setting.ChannelAffinityRule{{Name: "test", IncludeRuleName: true, ModelRegex: []string{"^gpt-"}, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}}}}}
	service.ClearChannelAffinityCacheAll()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ctx.Set("token_id", 101)
	_, _ = service.GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
	service.RecordChannelAffinity(ctx, 1)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	t.Cleanup(func() {
		*operation_setting.GetChannelAffinitySetting() = oldSetting
		model.DB, common.MemoryCacheEnabled = oldDB, oldMemory
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/option/channel_affinity_bindings", nil)
	GetChannelAffinityBindings(c)
	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &payload))
	require.False(t, payload.Success)
	require.Equal(t, "渠道亲和性绑定读取失败", payload.Message)
}
