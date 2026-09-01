package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupChannelCandidatesDB 建内存库表并插入测试渠道与 ability 数据。
// priority 各不相同以验证候选集覆盖全部优先级层级（非单层）。
func setupChannelCandidatesDB(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&Channel{}, &Ability{}))
	DB.Where("1 = 1").Delete(&Ability{})
	DB.Where("1 = 1").Delete(&Channel{})

	p1, p5, p9 := int64(1), int64(5), int64(9)
	channels := []*Channel{
		{Id: 1, Name: "c1", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "gpt-4", Group: "default", Priority: &p9},
		{Id: 2, Name: "c2", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "gpt-4", Group: "default", Priority: &p5},
		{Id: 3, Name: "c3", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "gpt-4", Group: "default", Priority: &p1},
		{Id: 4, Name: "c4", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "gpt-4o", Group: "default", Priority: &p9},
		{Id: 5, Name: "c5", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "gpt-4", Group: "vip", Priority: &p9},
		{Id: 6, Name: "c6", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusManuallyDisabled, Models: "gpt-4", Group: "default", Priority: &p9},
		{Id: 7, Name: "c7", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "gpt-4,gpt-4o", Group: "default", Priority: &p9},
	}
	for _, ch := range channels {
		require.NoError(t, DB.Create(ch).Error)
		require.NoError(t, ch.AddAbilities(nil))
	}
}

// TestGetEnabledChannelIDsForGroupModel_EmptyGroup 计划核心断言：空入参守卫。
func TestGetEnabledChannelIDsForGroupModel_EmptyGroup(t *testing.T) {
	setupChannelCandidatesDB(t)
	assert.Equal(t, []int{}, GetEnabledChannelIDsForGroupModel("", "gpt-4", "/v1/chat/completions"))
}

// TestGetEnabledChannelIDsForGroupModel_MemoryCacheAllPriorities 内存缓存启用时
// 返回分组+模型下全部可用渠道（全部优先级层级，非单层）。
func TestGetEnabledChannelIDsForGroupModel_MemoryCacheAllPriorities(t *testing.T) {
	setupChannelCandidatesDB(t)

	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = prev
		InitChannelCache()
	})
	InitChannelCache()

	ids := GetEnabledChannelIDsForGroupModel("default", "gpt-4", "/v1/chat/completions")
	assert.ElementsMatch(t, []int{1, 2, 3, 7}, ids)
}

// TestGetEnabledChannelIDsForGroupModel_MemoryCacheDisabledDBFallback 内存缓存未启用时
// 走 DB 查询：enabled=true、按 group+model 过滤、去重 channel_id、覆盖全部优先级层级。
func TestGetEnabledChannelIDsForGroupModel_MemoryCacheDisabledDBFallback(t *testing.T) {
	setupChannelCandidatesDB(t)

	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = prev })

	ids := GetEnabledChannelIDsForGroupModel("default", "gpt-4", "/v1/chat/completions")
	assert.ElementsMatch(t, []int{1, 2, 3, 7}, ids)
}

// TestGetEnabledChannelIDsForGroupModel_GroupAndModelFilter 分组与模型过滤：
// 其它分组、其它模型的渠道不进入候选。
func TestGetEnabledChannelIDsForGroupModel_GroupAndModelFilter(t *testing.T) {
	setupChannelCandidatesDB(t)

	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = prev })

	assert.ElementsMatch(t, []int{5}, GetEnabledChannelIDsForGroupModel("vip", "gpt-4", "/v1/chat/completions"))
	assert.Empty(t, GetEnabledChannelIDsForGroupModel("other", "gpt-4", "/v1/chat/completions"))
	assert.ElementsMatch(t, []int{4, 7}, GetEnabledChannelIDsForGroupModel("default", "gpt-4o", "/v1/chat/completions"))
}

// TestGetEnabledChannelIDsForGroupModel_NormalizedModelFallback 内存缓存模式下
// 精确模型名无候选时回退 normalized model 名（与 GetRandomSatisfiedChannel 语义一致）。
// gpt-4o-gizmo-foo 归一化为 gpt-4o-gizmo-*（FormatMatchingModelName）。
func TestGetEnabledChannelIDsForGroupModel_NormalizedModelFallback(t *testing.T) {
	setupChannelCandidatesDB(t)
	p9 := int64(9)
	ch8 := &Channel{Id: 8, Name: "c8", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "gpt-4o-gizmo-*", Group: "default", Priority: &p9}
	require.NoError(t, DB.Create(ch8).Error)
	require.NoError(t, ch8.AddAbilities(nil))

	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = prev
		InitChannelCache()
	})
	InitChannelCache()

	ids := GetEnabledChannelIDsForGroupModel("default", "gpt-4o-gizmo-foo", "/v1/chat/completions")
	assert.ElementsMatch(t, []int{8}, ids)
}

// TestGetEnabledChannelIDsForGroupModel_AdvancedCustomPathFilter Advanced Custom 渠道
// 经路径过滤：路由匹配 requestPath+model 的保留，不匹配的被剔除。
func TestGetEnabledChannelIDsForGroupModel_AdvancedCustomPathFilter(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&Channel{}, &Ability{}))
	DB.Where("1 = 1").Delete(&Ability{})
	DB.Where("1 = 1").Delete(&Channel{})

	p9 := int64(9)
	advancedOther := `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/messages","models":["gpt-4"]}]}}`
	advancedMatch := `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/chat/completions","models":["gpt-4"]}]}}`

	channels := []*Channel{
		{Id: 11, Name: "adv-mismatch", Type: constant.ChannelTypeAdvancedCustom, Status: common.ChannelStatusEnabled, Models: "gpt-4", Group: "default", Priority: &p9, OtherSettings: advancedOther},
		{Id: 12, Name: "adv-match", Type: constant.ChannelTypeAdvancedCustom, Status: common.ChannelStatusEnabled, Models: "gpt-4", Group: "default", Priority: &p9, OtherSettings: advancedMatch},
		{Id: 13, Name: "plain", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "gpt-4", Group: "default", Priority: &p9},
	}
	for _, ch := range channels {
		require.NoError(t, DB.Create(ch).Error)
		require.NoError(t, ch.AddAbilities(nil))
	}

	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = prev })

	ids := GetEnabledChannelIDsForGroupModel("default", "gpt-4", "/v1/chat/completions")
	assert.ElementsMatch(t, []int{12, 13}, ids, "advanced custom channel without matching route should be excluded")
}

// TestGetEnabledChannelIDsForGroupModel_NoCandidates 无候选时返回空切片（非 nil），
// 供上层统一按空候选处理。
func TestGetEnabledChannelIDsForGroupModel_NoCandidates(t *testing.T) {
	setupChannelCandidatesDB(t)

	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = prev })

	ids := GetEnabledChannelIDsForGroupModel("default", "unknown-model", "/v1/chat/completions")
	require.NotNil(t, ids)
	assert.Empty(t, ids)
}
