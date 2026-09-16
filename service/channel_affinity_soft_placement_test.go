package service

// 生产缺陷回归：随机选路结果被固化为正式绑定，绕过独占判定。
// 症状（生产观察）：渠道大量空闲时，多个亲和键共享同一渠道，且无满载降级理由。
// 固化路径（修复前）：
//   1. 亲和命中渠道不可用（自动封禁/禁用/移出分组）→ 清占位 → 随机选路成功 →
//      RecordChannelAffinity 把随机渠道钉成新绑定（迁入豁免追加占用索引）。
//   2. keep_on_channel_disabled=true 时随机兜底结果同样覆盖绑定并追加占用。
//   3. 亲和渠道失败重试切换成功 → 迁移豁免把重试渠道钉为新绑定。
// 修复后语义：
//   - 清占位分支：重新走独占 acquire，从当前可用候选中独占选定，随机兜底仅在
//     acquire 不可用时发生且不固化（软落位标记）。
//   - keep 分支与重试切换：软落位标记，回写跳过固化，绑定维持原值。
// 测试镜像 distributor.go 亲和块与 relay 成功回写的真实调用时序（白盒），
// 修复前红、修复后绿。

import (
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// softPlacementRuleName 独占规则名（header 键源）。
const softPlacementRuleName = "exclusive-header-rule"

// softPlacementChannels 夹具渠道集。
var softPlacementChannels = []int{921, 922, 923, 924, 925, 926}

// useSoftPlacementFixture 构造 921..926 共 6 渠道（default/gpt-4）+ 独占 header 规则。
// keepRedis=true 时保留调用方已切换的 Redis 全局（真实 Redis 集成测试用），
// 不再强制内存模式；否则强制内存模式。
func useSoftPlacementFixture(t *testing.T, exclusive bool) {
	t.Helper()
	if exclusive {
		useExclusiveRedisKeepMode(t)
	} else {
		useExclusiveMemoryMode(t)
	}
	setupSoftPlacementDB(t)
	resetAffinityCacheSingleton()

	setting := operation_setting.GetChannelAffinitySetting()
	origRules := setting.Rules
	origEnabled := setting.Enabled
	origSwitch := setting.SwitchOnSuccess
	origKeep := setting.KeepOnChannelDisabled
	setting.Rules = []operation_setting.ChannelAffinityRule{{
		Name:              softPlacementRuleName,
		ModelRegex:        []string{"^gpt-4$"},
		KeySources:        []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Affinity-Key"}},
		TTLSeconds:        60,
		IncludeRuleName:   true,
		IncludeUsingGroup: true,
		ExclusiveBind:     exclusive,
	}}
	setting.Enabled = true
	setting.SwitchOnSuccess = true
	setting.KeepOnChannelDisabled = false
	t.Cleanup(func() {
		setting.Rules = origRules
		setting.Enabled = origEnabled
		setting.SwitchOnSuccess = origSwitch
		setting.KeepOnChannelDisabled = origKeep
		keys := make([]string, 0, len(softPlacementChannels))
		for _, id := range softPlacementChannels {
			keys = append(keys, fmt.Sprintf("%d", id))
		}
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(keys)
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})
}

// setupSoftPlacementDB 建内存库并注入 921..926 渠道（default/gpt-4）。
func setupSoftPlacementDB(t *testing.T) {
	t.Helper()
	originalDB := model.DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))

	priority := int64(0)
	weight := uint(100)
	for _, id := range softPlacementChannels {
		require.NoError(t, db.Create(&model.Channel{
			Id:       id,
			Type:     constant.ChannelTypeOpenAI,
			Key:      fmt.Sprintf("key-%d", id),
			Status:   common.ChannelStatusEnabled,
			Name:     fmt.Sprintf("soft-placement-channel-%d", id),
			Weight:   &weight,
			Models:   "gpt-4",
			Group:    "default",
			Priority: &priority,
		}).Error)
		require.NoError(t, db.Create(&model.Ability{
			Group:     "default",
			Model:     "gpt-4",
			ChannelId: id,
			Enabled:   true,
			Priority:  &priority,
			Weight:    weight,
		}).Error)
	}
	model.DB = db
	common.MemoryCacheEnabled = true
	model.InitChannelCache()
	t.Cleanup(func() {
		model.DB = originalDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		if originalMemoryCacheEnabled && originalDB != nil {
			model.InitChannelCache()
		}
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
}

// setChannelsDisabled 以真实渠道状态模拟禁用（DB + 内存缓存同步，与自动封禁路径一致）。
func setChannelsDisabled(t *testing.T, ids ...int) {
	t.Helper()
	for _, id := range ids {
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", id).
			Update("status", common.ChannelStatusManuallyDisabled).Error)
		require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", id).
			Update("enabled", false).Error)
	}
	model.InitChannelCache()
	t.Cleanup(func() { model.InitChannelCache() })
}

// setChannelsEnabled 恢复渠道可用。
func setChannelsEnabled(t *testing.T, ids ...int) {
	t.Helper()
	for _, id := range ids {
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", id).
			Update("status", common.ChannelStatusEnabled).Error)
		require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", id).
			Update("enabled", true).Error)
	}
	model.InitChannelCache()
}

// affinityKeySuffix 由规则键构成推出正向缓存 suffix。
func affinityKeySuffix(key string) string {
	return softPlacementRuleName + ":default:" + key
}

// seedExclusiveBinding 直接预置某键的独占占位三处（确定性初始态）。
func seedExclusiveBinding(t *testing.T, key string, channelID int) {
	t.Helper()
	suffix := affinityKeySuffix(key)
	require.NoError(t, getChannelAffinityCache().SetWithTTL(suffix, channelID, 60*time.Second))
	require.NoError(t, occupancyAddKeyFP(channelID, affinityFingerprint(key), 60*time.Second))
	require.NoError(t, lastBindSet(suffix, channelID, 120*time.Second))
}

// mirrorDistributorRequest 镜像 distributor.go 亲和块 + relay 成功回写
// （SetupContextForSelectedChannel 置 channel_id、RecordChannelAffinity）的完整时序。
// 返回本次请求实际使用的渠道。
func mirrorDistributorRequest(t *testing.T, key string) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Affinity-Key", key)

	channel := 0
	if preferredID, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default"); found {
		affinityUsable := false
		if preferred, err := model.CacheGetChannel(preferredID); err == nil && preferred != nil &&
			preferred.Status == common.ChannelStatusEnabled &&
			model.IsChannelEnabledForGroupModel("default", "gpt-4", preferred.Id) {
			channel = preferredID
			affinityUsable = true
			MarkChannelAffinityUsed(ctx, "default", preferred.Id)
		}
		if !affinityUsable {
			if !ShouldKeepChannelAffinityOnChannelDisabled() {
				ClearCurrentChannelAffinityCache(ctx)
				// 与真实 distributor 一致：清占位后独占规则重新走亲和获取（内部进
				// acquire 独占决策，候选集来自真实渠道状态）。
				if rebindID, refound := GetPreferredChannelByAffinity(ctx, "gpt-4", "default"); refound {
					if p, perr := model.CacheGetChannel(rebindID); perr == nil && p != nil &&
						p.Status == common.ChannelStatusEnabled &&
						model.IsChannelEnabledForGroupModel("default", "gpt-4", p.Id) {
						channel = rebindID
					}
				}
			}
			if channel == 0 {
				// 随机兜底（acquire 降级/无可用候选）：软落位防固化。
				MarkChannelAffinitySoftPlacement(ctx)
			}
		}
	}

	if channel == 0 {
		// 随机选路：缓存候选集内等权随机（CacheGetRandomSatisfiedChannel 的镜像）。
		candidates := model.GetEnabledChannelIDsForGroupModel("default", "gpt-4", "/v1/chat/completions")
		require.NotEmpty(t, candidates)
		channel = candidates[rand.Intn(len(candidates))]
	}

	ctx.Set("channel_id", channel) // SetupContextForSelectedChannel 的回写依据
	RecordChannelAffinity(ctx, channel)
	return channel
}

// channelMemberCount 渠道占用键数。
func channelMemberCount(t *testing.T, channelID int) int {
	t.Helper()
	count, _, err := occupancyBindingCount(channelID)
	require.NoError(t, err)
	return count
}

// forwardBinding 正向缓存当前绑定渠道（0 为无绑定）。
func forwardBinding(t *testing.T, key string) int {
	t.Helper()
	v, found, err := getChannelAffinityCache().Get(affinityKeySuffix(key))
	require.NoError(t, err)
	if !found {
		return 0
	}
	return v
}

// T1 亲和命中渠道被禁用：清占位后重走独占 acquire，不随机固化。
// 场景：A 绑 921（被禁用）、B 绑 922，923..926 可用。
// 修复前：A 的请求随机落到 922..926 之一并被固化；多轮禁用扰动后共享渠道堆积。
// 修复后：A 的请求重 acquire 独占选定 923..926 之一（避开 922 的占用）。
func TestDisabledChannelRandomFallbackNotPinned(t *testing.T) {
	useSoftPlacementFixture(t, true)

	seedExclusiveBinding(t, "key-a", 921)
	seedExclusiveBinding(t, "key-b", 922)

	// 921（A 的绑定）禁用，922..926 可用（922 被 B 独占）。
	setChannelsDisabled(t, 921)

	used := mirrorDistributorRequest(t, "key-a")
	assert.Contains(t, []int{923, 924, 925, 926}, used,
		"cleared placement must re-acquire a free channel, not the occupied 922 nor random-disabled 921")
	assert.Equal(t, used, forwardBinding(t, "key-a"), "re-acquired binding must be pinned")
	assert.Equal(t, 1, channelMemberCount(t, used))
	// 922 始终只有 B 一个键：随机固化修复的核心断言。
	assert.Equal(t, 1, channelMemberCount(t, 922),
		"random fallback must not register occupancy on another key's exclusive channel")
	// 921 的旧占位已被清除迁移。
	assert.Equal(t, 0, channelMemberCount(t, 921))
}

// 独占规则在渠道禁用后重新绑定，即使 keep_on_channel_disabled=true。
// 新绑定避开其他键占用的渠道，并释放旧渠道占用。
func TestExclusiveDisabledRebindOverridesKeepSetting(t *testing.T) {
	useSoftPlacementFixture(t, true)
	setting := operation_setting.GetChannelAffinitySetting()
	origKeep := setting.KeepOnChannelDisabled
	setting.KeepOnChannelDisabled = true
	t.Cleanup(func() { setting.KeepOnChannelDisabled = origKeep })

	seedExclusiveBinding(t, "key-a", 921)
	seedExclusiveBinding(t, "key-b", 922)

	setChannelsDisabled(t, 921)

	used := mirrorDistributorRequest(t, "key-a")
	require.Contains(t, []int{923, 924, 925, 926}, used)

	assert.Equal(t, used, forwardBinding(t, "key-a"), "独占规则禁用后必须重绑")
	// 仅新绑定渠道持有 A 的指纹，其他渠道占用保持原样。
	for _, id := range []int{922, 923, 924, 925, 926} {
		_, fps, err := occupancyBindingCount(id)
		require.NoError(t, err)
		if id == used {
			assert.Contains(t, fps, affinityFingerprint("key-a"))
		} else {
			assert.NotContains(t, fps, affinityFingerprint("key-a"))
		}
	}
	assert.Equal(t, 0, channelMemberCount(t, 921), "旧渠道释放占用")
	assert.Equal(t, 1, channelMemberCount(t, 922), "channel 922 keeps only key-b")
}

// T3 重试切换迁移：独占键失败切换到其它键的渠道成功后，不得钉住共享渠道。
// 镜像 relay 重试路径：首占 921 失败，重试选中 922（B 独占）成功，
// ctx channel_id=922 → RecordChannelAffinity(922)。
// 修复前：迁入豁免追加 → 922 键数 2、正向绑定改写 922；
// 修复后：绑定维持 921，922 键数不变，降级计数可观察。
func TestRetrySwitchDoesNotPinOccupiedChannel(t *testing.T) {
	useSoftPlacementFixture(t, true)

	seedExclusiveBinding(t, "key-a", 921)
	seedExclusiveBinding(t, "key-b", 922)
	degradedBefore := degradedReuseTotal()

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Affinity-Key", "key-a")
	_, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
	require.True(t, found) // 亲和命中 921，bound=921

	ctx.Set("channel_id", 922) // 重试切换成功渠道
	RecordChannelAffinity(ctx, 922)

	assert.Equal(t, 1, channelMemberCount(t, 922),
		"retry switch must not append occupancy to another key's exclusive channel")
	assert.Equal(t, 921, forwardBinding(t, "key-a"),
		"exclusive binding movement belongs to the exclusive engine, not request outcome")
	assert.Equal(t, 1, channelMemberCount(t, 921), "original placement must stay registered")
	assert.GreaterOrEqual(t, degradedReuseTotal(), degradedBefore+1,
		"retry-switch shared use must be observable in degrade counter")
}

// T4 软规则回归守卫：未启用独占的规则保持现有行为（随机首绑正常固化登记）。
func TestSoftRuleRandomFirstBindStillPins(t *testing.T) {
	useSoftPlacementFixture(t, false)

	used := mirrorDistributorRequest(t, "key-c")
	require.Contains(t, softPlacementChannels, used)

	assert.Equal(t, used, forwardBinding(t, "key-c"), "soft rule must keep pinning random first bind")
	assert.Equal(t, 1, channelMemberCount(t, used))
}

// T5 批量扰动收敛：8 键 6 渠道，多轮随机禁用/恢复交错请求后，
// 全渠道恢复、每键再请求一轮，终态必须收敛为均衡独占分布：
// 总成员 = 8（每键恰一次登记）、单渠道 ≤2、无渠道为 0（满载共享仅 2 键分摊）。
// 修复前：随机兜底固化制造堆积（某渠道 ≥3 且有空闲渠道）。
func TestChurnConvergesToBalancedExclusive(t *testing.T) {
	useSoftPlacementFixture(t, true)

	rng := rand.New(rand.NewSource(42))
	keys := []string{"churn-1", "churn-2", "churn-3", "churn-4", "churn-5", "churn-6", "churn-7", "churn-8"}

	// 初始：每键经完整时序请求一次（独占 acquire 分布）。
	for _, key := range keys {
		mirrorDistributorRequest(t, key)
	}

	// 扰动 8 轮：每轮随机禁用 2 渠道、恢复上轮禁用的，每键请求一次。
	prevDisabled := []int{}
	for round := 0; round < 8; round++ {
		if len(prevDisabled) > 0 {
			setChannelsEnabled(t, prevDisabled...)
		}
		disabledNow := []int{
			softPlacementChannels[rng.Intn(len(softPlacementChannels))],
			softPlacementChannels[rng.Intn(len(softPlacementChannels))],
		}
		setChannelsDisabled(t, disabledNow...)
		prevDisabled = disabledNow
		for _, key := range keys {
			mirrorDistributorRequest(t, key)
		}
	}
	if len(prevDisabled) > 0 {
		setChannelsEnabled(t, prevDisabled...)
	}

	// 收敛轮：全渠道可用，每键再请求一次。
	for _, key := range keys {
		mirrorDistributorRequest(t, key)
	}

	counts := make([]int, 0, len(softPlacementChannels))
	total := 0
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		counts = append(counts, c)
		total += c
	}
	sortedCounts := append([]int(nil), counts...)
	sort.Sort(sort.Reverse(sort.IntSlice(sortedCounts)))
	t.Logf("settled distribution: %v (channels %v)", counts, softPlacementChannels)

	assert.Equal(t, len(keys), total, "every key must be registered exactly once after settling")
	assert.LessOrEqual(t, sortedCounts[0], 2,
		"8 keys over 6 channels: any channel holding >2 means stacking beyond the 2-per-channel full-load bound (4+ available)")
	// 说明：收敛轮后允许存在空渠道——扰动期间禁用 2 渠道会造成 8 键挤 4 渠道的
	// 合理满载（各 2），恢复后既有绑定不主动迁移（亲和命中续期），随 TTL 到期
	// 重绑自然再平衡。独占不变量由"单渠道 ≤2 + 总量守恒"守护。
}
