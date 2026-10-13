package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupRouteCleanupTest 初始化内存库与亲和规则，写入 token→channel 绑定。
// 渠道路由相关配置（models/group/weight/priority/tag）被管理操作变更后，
// 绑定应按渠道三批同清（CHL_001 5.2.2 第7条）。
func setupRouteCleanupTest(t *testing.T, channels []*model.Channel, tokenIDs []int) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Channel{}, &model.Token{}, &model.Ability{}))
	oldDB, oldLogDB, oldMemory, oldRedis := model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled
	oldSetting := *operation_setting.GetChannelAffinitySetting()
	model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled = db, db, true, false
	*operation_setting.GetChannelAffinitySetting() = operation_setting.ChannelAffinitySetting{Enabled: true, DefaultTTLSeconds: 60,
		Rules: []operation_setting.ChannelAffinityRule{{Name: "route-cleanup", IncludeRuleName: true, IncludeModelName: true,
			ModelRegex: []string{"^gpt-"}, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}}}}}
	service.ClearChannelAffinityCacheAll()
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "u", Role: 1}).Error)
	for _, ch := range channels {
		require.NoError(t, db.Create(ch).Error)
		// InitChannelCache 按 abilities 的 group 集合建二级索引，缺行会命中 nil map
		require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Group: ch.Group, Model: "gpt-4", Enabled: true}).Error)
	}
	for _, tokenID := range tokenIDs {
		require.NoError(t, db.Create(&model.Token{Id: tokenID, UserId: 1, Name: "令牌", Key: fmt.Sprintf("sk-test-%d", tokenID)}).Error)
	}
	t.Cleanup(func() {
		ids := make([]int, 0, len(channels))
		for _, ch := range channels {
			ids = append(ids, ch.Id)
		}
		service.ClearChannelAffinityRuntimeByChannelIDs(ids)
		service.ClearChannelAffinityCacheAll()
		*operation_setting.GetChannelAffinitySetting() = oldSetting
		model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled = oldDB, oldLogDB, oldMemory, oldRedis
		if oldMemory && oldDB != nil {
			model.InitChannelCache()
		}
		if sqlDB, closeErr := db.DB(); closeErr == nil {
			_ = sqlDB.Close()
		}
	})
}

// recordTestBinding 按夹具规则写入一条 token→channel 的 gpt-4 亲和绑定。
func recordTestBinding(t *testing.T, tokenID, channelID int) {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ctx.Set("token_id", tokenID)
	_, _ = service.GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
	service.RecordChannelAffinity(ctx, channelID)
}

// boundChannelIDs 返回当前仍有亲和绑定的渠道 ID 集合。
func boundChannelIDs(t *testing.T) map[int]bool {
	t.Helper()
	bindings, err := service.GetChannelAffinityBindings()
	require.NoError(t, err)
	result := make(map[int]bool)
	for _, b := range bindings {
		if len(b.Tokens) > 0 {
			result[b.ChannelID] = true
		}
	}
	return result
}

func callUpdateChannel(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", 1)
	c.Set("role", 100)
	UpdateChannel(c)
	return w
}

func callEditTagChannels(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", 1)
	c.Set("role", 100)
	EditTagChannels(c)
	return w
}

func decodeSuccess(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	var payload struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &payload))
	return payload.Success
}

func TestUpdateChannelRouteChangeClearsAffinity(t *testing.T) {
	setupRouteCleanupTest(t, []*model.Channel{
		{Id: 9811, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Name: "渠道甲", Key: "test", Models: "gpt-4", Group: "default"},
		{Id: 9812, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Name: "渠道乙", Key: "test", Models: "gpt-4", Group: "default"},
	}, []int{201, 202})
	recordTestBinding(t, 201, 9811)
	recordTestBinding(t, 202, 9812)

	// group 属路由相关列：变更后仅渠道 9811 的绑定被三批同清
	w := callUpdateChannel(t, `{"id":9811,"name":"渠道甲","group":"vip"}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, decodeSuccess(t, w))

	var group string
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 9811).Pluck("group", &group).Error)
	require.Equal(t, "vip", group, "编辑应已生效")

	remaining := boundChannelIDs(t)
	assert.False(t, remaining[9811], "路由列变更后该渠道绑定应被清理")
	assert.True(t, remaining[9812], "未编辑渠道的绑定应保留")
}

func TestUpdateChannelNonRouteChangeKeepsAffinity(t *testing.T) {
	setupRouteCleanupTest(t, []*model.Channel{
		{Id: 9811, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Name: "渠道甲", Key: "test", Models: "gpt-4", Group: "default"},
	}, []int{201})
	recordTestBinding(t, 201, 9811)
	require.True(t, boundChannelIDs(t)[9811], "前置条件：绑定应已写入")

	// name 非路由列：不应触发亲和清理
	w := callUpdateChannel(t, `{"id":9811,"name":"渠道甲改名"}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, decodeSuccess(t, w))

	assert.True(t, boundChannelIDs(t)[9811], "非路由列编辑不应清理绑定")
}

func TestEditTagChannelsRouteChangeClearsAffinity(t *testing.T) {
	setupRouteCleanupTest(t, []*model.Channel{
		{Id: 9811, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Name: "渠道甲", Key: "test", Models: "gpt-4", Group: "default", Tag: common.GetPointer("cleanup-tag")},
		{Id: 9812, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Name: "渠道乙", Key: "test", Models: "gpt-4", Group: "default", Tag: common.GetPointer("cleanup-tag")},
		{Id: 9813, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
			Name: "渠道丙", Key: "test", Models: "gpt-4", Group: "default"},
	}, []int{201, 202, 203})
	recordTestBinding(t, 201, 9811)
	recordTestBinding(t, 202, 9812)
	recordTestBinding(t, 203, 9813)

	// 按标签编辑 group：该标签下全部渠道的绑定被清理，标签外渠道不受影响
	w := callEditTagChannels(t, `{"tag":"cleanup-tag","groups":"vip"}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, decodeSuccess(t, w))

	remaining := boundChannelIDs(t)
	assert.False(t, remaining[9811], "标签内渠道绑定应被清理")
	assert.False(t, remaining[9812], "标签内渠道绑定应被清理")
	assert.True(t, remaining[9813], "标签外渠道绑定应保留")
}
