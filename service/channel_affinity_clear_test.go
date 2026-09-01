package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 T6：管理员清空三批同清（正向缓存、反向占用索引、最近绑定记录，
// SSOT 5.2.2 第4条、5.2.4 规则2、5.2.5 异常、4.2.3 清空按钮行为）。

// useClearAffinityTest 夹具：内存模式存储 + 重置三套缓存单例 + 临时注入
// ruleA/ruleB 两条 include_rule_name=true 的规则（按规则清空的前置校验依赖）。
// 用例结束恢复设置并清空三套缓存残留。
func useClearAffinityTest(t *testing.T) {
	t.Helper()
	useExclusiveMemoryMode(t)
	resetAffinityCacheSingleton()

	setting := operation_setting.GetChannelAffinitySetting()
	originalRules := setting.Rules
	setting.Rules = []operation_setting.ChannelAffinityRule{
		{
			Name:            "ruleA",
			ModelRegex:      []string{"^gpt-.*$"},
			KeySources:      []operation_setting.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}},
			IncludeRuleName: true,
		},
		{
			Name:            "ruleB",
			ModelRegex:      []string{"^claude-.*$"},
			KeySources:      []operation_setting.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}},
			IncludeRuleName: true,
		},
	}
	t.Cleanup(func() {
		setting.Rules = originalRules
		resetAffinityCacheSingleton()
		purgeAffinityCachesForTest()
	})
}

// purgeAffinityCachesForTest 清空三套亲和缓存全部条目（夹具收尾用）。
func purgeAffinityCachesForTest() {
	_, _ = getChannelAffinityCache().DeleteMany(append([]string{}, collectForwardKeysForTest()...))
	occKeys, _ := getChannelAffinityOccupancyCache().Keys()
	if len(occKeys) > 0 {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(occKeys)
	}
	lastKeys, _ := getChannelAffinityLastBindCache().Keys()
	if len(lastKeys) > 0 {
		_, _ = getChannelAffinityLastBindCache().DeleteMany(lastKeys)
	}
	occupancyMemExpireAt.Range(func(key, _ any) bool {
		occupancyMemExpireAt.Delete(key)
		return true
	})
}

// collectForwardKeysForTest 收集正向缓存全键（full key）。
func collectForwardKeysForTest() []string {
	keys, _ := getChannelAffinityCache().Keys()
	return keys
}

// TestClearChannelAffinityCacheAllClearsExclusive 计划核心断言：
// 正向 SetWithTTL 两键、occupancyAddKeyFP 两渠道、lastBindSet 两键造数后，
// ClearChannelAffinityCacheAll() 三批同清（SSOT 5.2.2 第4条、5.2.4 规则2）。
func TestClearChannelAffinityCacheAllClearsExclusive(t *testing.T) {
	useClearAffinityTest(t)
	ttl := 10 * time.Second
	forward := getChannelAffinityCache()

	// 正向两键（suffix 口径写入）。
	require.NoError(t, forward.SetWithTTL("ruleA:default:key-a", 301, ttl))
	require.NoError(t, forward.SetWithTTL("ruleB:default:key-b", 302, ttl))
	// 占用索引两渠道。
	require.NoError(t, occupancyAddKeyFP(301, affinityFingerprint("key-a"), ttl))
	require.NoError(t, occupancyAddKeyFP(302, affinityFingerprint("key-b"), ttl))
	// 最近绑定两键。
	require.NoError(t, lastBindSet("ruleA:default:key-a", 301, 2*ttl))
	require.NoError(t, lastBindSet("ruleB:default:key-b", 302, 2*ttl))

	deleted := ClearChannelAffinityCacheAll()
	assert.Equal(t, 2, deleted, "返回值语义沿用现状：正向删除条数")

	for _, id := range []int{301, 302} {
		count, fps, err := occupancyBindingCount(id)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "channel %d occupancy should be cleared", id)
		assert.Empty(t, fps)
	}
	for _, suffix := range []string{"ruleA:default:key-a", "ruleB:default:key-b"} {
		_, found, err := lastBindGet(suffix)
		require.NoError(t, err)
		assert.False(t, found, "last bind %s should be cleared", suffix)
		_, found, err = forward.Get(suffix)
		require.NoError(t, err)
		assert.False(t, found, "forward %s should be cleared", suffix)
	}
}

// TestClearChannelAffinityCacheByRuleNameClearsExclusive 计划核心断言：
// ruleA（include_rule_name=true）与 ruleB 两前缀造数，按 ruleA 清空后
// ruleA 前缀三处全清、ruleB 前缀正向与索引仍在，deleted 等于 ruleA 正向条数。
func TestClearChannelAffinityCacheByRuleNameClearsExclusive(t *testing.T) {
	useClearAffinityTest(t)
	ttl := 10 * time.Second
	forward := getChannelAffinityCache()

	require.NoError(t, forward.SetWithTTL("ruleA:default:key-a", 301, ttl))
	require.NoError(t, forward.SetWithTTL("ruleA:default:key-b", 301, ttl))
	require.NoError(t, forward.SetWithTTL("ruleB:default:key-c", 302, ttl))
	require.NoError(t, occupancyAddKeyFP(301, affinityFingerprint("key-a"), ttl))
	require.NoError(t, occupancyAddKeyFP(301, affinityFingerprint("key-b"), ttl))
	require.NoError(t, occupancyAddKeyFP(302, affinityFingerprint("key-c"), ttl))
	require.NoError(t, lastBindSet("ruleA:default:key-a", 301, 2*ttl))
	require.NoError(t, lastBindSet("ruleB:default:key-c", 302, 2*ttl))

	deleted, err := ClearChannelAffinityCacheByRuleName("ruleA")
	require.NoError(t, err)
	assert.Equal(t, 2, deleted, "deleted 等于 ruleA 正向条数")

	// ruleA 三处全清。
	_, found, err := forward.Get("ruleA:default:key-a")
	require.NoError(t, err)
	assert.False(t, found)
	_, found, err = forward.Get("ruleA:default:key-b")
	require.NoError(t, err)
	assert.False(t, found)
	_, found, err = lastBindGet("ruleA:default:key-a")
	require.NoError(t, err)
	assert.False(t, found)
	count, fps, err := occupancyBindingCount(301)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, fps)

	// ruleB 前缀正向与索引仍在。
	_, found, err = forward.Get("ruleB:default:key-c")
	require.NoError(t, err)
	assert.True(t, found, "ruleB forward entry must survive ruleA clear")
	count, fps, err = occupancyBindingCount(302)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{affinityFingerprint("key-c")}, fps)
}

// TestClearByRuleKeepsOtherRuleMemberOnSharedChannel 计划核心断言：
// ruleA 与 ruleB 的键均登记到同一渠道 801（满载复用形态），按 ruleA 清空后
// occupancyBindingCount(801) 仍返回键数 1 且仅含 ruleB 键的指纹（成员级移除，
// 其它规则同渠道成员不误删，04 文档 §6.2）。
func TestClearByRuleKeepsOtherRuleMemberOnSharedChannel(t *testing.T) {
	useClearAffinityTest(t)
	ttl := 10 * time.Second
	forward := getChannelAffinityCache()
	fpA := affinityFingerprint("shared-key-a")
	fpB := affinityFingerprint("shared-key-b")

	require.NoError(t, forward.SetWithTTL("ruleA:default:shared-key-a", 801, ttl))
	require.NoError(t, forward.SetWithTTL("ruleB:default:shared-key-b", 801, ttl))
	require.NoError(t, occupancyAddKeyFP(801, fpA, ttl))
	require.NoError(t, occupancyAddKeyFP(801, fpB, ttl))

	_, err := ClearChannelAffinityCacheByRuleName("ruleA")
	require.NoError(t, err)

	count, fps, err := occupancyBindingCount(801)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "member-level removal: only ruleB fingerprint stays")
	assert.ElementsMatch(t, []string{fpB}, fps)

	// ruleB 正向仍在。
	_, found, err := forward.Get("ruleB:default:shared-key-b")
	require.NoError(t, err)
	assert.True(t, found)
}

// TestClearByRuleNameValidationErrors 校验逻辑不变（03 文档 3.3 错误表）：
// 空规则名、未知规则名、未启用 include_rule_name 均报错且无清空副作用。
func TestClearByRuleNameValidationErrors(t *testing.T) {
	useClearAffinityTest(t)
	ttl := 10 * time.Second
	forward := getChannelAffinityCache()

	_, err := ClearChannelAffinityCacheByRuleName("")
	require.Error(t, err)

	_, err = ClearChannelAffinityCacheByRuleName("no-such-rule")
	require.Error(t, err)

	// 临时把 ruleB 的 include_rule_name 关掉：按规则清空应报错。
	setting := operation_setting.GetChannelAffinitySetting()
	ruleB := &setting.Rules[1]
	require.Equal(t, "ruleB", ruleB.Name)
	ruleB.IncludeRuleName = false
	defer func() { ruleB.IncludeRuleName = true }()

	require.NoError(t, forward.SetWithTTL("ruleB:default:key-x", 302, ttl))
	_, err = ClearChannelAffinityCacheByRuleName("ruleB")
	require.Error(t, err)

	_, found, getErr := forward.Get("ruleB:default:key-x")
	require.NoError(t, getErr)
	assert.True(t, found, "validation failure must not clear forward entries")
}

// TestRetryOnce retryOnce 语义（SSOT 5.2.5）：首次成功直接返回；
// 首次失败重试一次成功返回 nil；两次均失败返回最后一次错误。
func TestRetryOnce(t *testing.T) {
	attempts := 0
	require.NoError(t, retryOnce(func() error {
		attempts++
		return nil
	}))
	assert.Equal(t, 1, attempts)

	attempts = 0
	require.NoError(t, retryOnce(func() error {
		attempts++
		if attempts == 1 {
			return errRetryOnceProbe
		}
		return nil
	}))
	assert.Equal(t, 2, attempts)

	attempts = 0
	err := retryOnce(func() error {
		attempts++
		return errRetryOnceProbe
	})
	assert.Error(t, err)
	assert.Equal(t, 2, attempts)
}

// errRetryOnceProbe 为 retryOnce 用例的哨兵错误。
var errRetryOnceProbe = errors.New("probe")

// TestClearByRuleNameNoForwardEntries 按规则清空前缀下无正向条目时，
// deleted 为 0 且无索引副作用（正常空分支）。
func TestClearByRuleNameNoForwardEntries(t *testing.T) {
	useClearAffinityTest(t)

	deleted, err := ClearChannelAffinityCacheByRuleName("ruleA")
	require.NoError(t, err)
	assert.Equal(t, 0, deleted)

	keys := collectForwardKeysForTest()
	assert.Empty(t, keys)
}

// TestClearExclusiveRuntimeAllPurgesBothNamespaces clearExclusiveRuntimeAll
// 直接调用：occupancy 与 lastBind 两命名空间整体清空（含 occupancyMemExpireAt
// 过期时刻表的同步清理）。
func TestClearExclusiveRuntimeAllPurgesBothNamespaces(t *testing.T) {
	useClearAffinityTest(t)
	ttl := 10 * time.Second

	require.NoError(t, occupancyAddKeyFP(311, affinityFingerprint("k1"), ttl))
	require.NoError(t, occupancyAddKeyFP(322, affinityFingerprint("k2"), ttl))
	require.NoError(t, lastBindSet("ruleA:default:k1", 311, 2*ttl))

	clearExclusiveRuntimeAll()

	for _, id := range []int{311, 322} {
		count, fps, err := occupancyBindingCount(id)
		require.NoError(t, err)
		assert.Equal(t, 0, count)
		assert.Empty(t, fps)
	}
	_, found, err := lastBindGet("ruleA:default:k1")
	require.NoError(t, err)
	assert.False(t, found)

	occupancyMemExpireAt.Range(func(key, _ any) bool {
		t.Errorf("occupancy expire map should be empty, got key %v", key)
		return false
	})
}

// TestClearByRuleNameValueWithColon 亲和键值含冒号时（指纹推导候选段逐一尝试），
// 按规则清空仍能移除占用成员：值段 key:1 生成的指纹从渠道条目移除，
// 规则名相同但值不同的其它键不受影响。
func TestClearByRuleNameValueWithColon(t *testing.T) {
	useClearAffinityTest(t)
	ttl := 10 * time.Second
	forward := getChannelAffinityCache()

	require.NoError(t, forward.SetWithTTL("ruleA:default:key:1", 331, ttl))
	require.NoError(t, forward.SetWithTTL("ruleA:default:plain", 331, ttl))
	require.NoError(t, occupancyAddKeyFP(331, affinityFingerprint("key:1"), ttl))
	require.NoError(t, occupancyAddKeyFP(331, affinityFingerprint("plain"), ttl))

	_, err := ClearChannelAffinityCacheByRuleName("ruleA")
	require.NoError(t, err)

	count, fps, err := occupancyBindingCount(331)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, fps)
}

// TestClearByRuleNameSkipsUnresolvableFingerprint 正向键存在但无法定位指纹
// （occupancy 条目缺失）时跳过该键不报错（残留依赖 TTL 收敛，SSOT 5.2.5）。
func TestClearByRuleNameSkipsUnresolvableFingerprint(t *testing.T) {
	useClearAffinityTest(t)
	ttl := 10 * time.Second
	forward := getChannelAffinityCache()

	// 正向有键但占用索引无任何对应条目：指纹推导无命中，属可跳过路径。
	require.NoError(t, forward.SetWithTTL("ruleA:default:ghost", 341, ttl))
	require.NoError(t, occupancyAddKeyFP(351, affinityFingerprint("unrelated"), ttl))

	deleted, err := ClearChannelAffinityCacheByRuleName("ruleA")
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	// 无关渠道成员不受影响。
	count, fps, err := occupancyBindingCount(351)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{affinityFingerprint("unrelated")}, fps)
}

// TestClearExclusiveRuntimeByForwardKeysEmpty 空键列表直接返回，无副作用。
func TestClearExclusiveRuntimeByForwardKeysEmpty(t *testing.T) {
	useClearAffinityTest(t)

	clearExclusiveRuntimeByForwardKeys(nil, nil)
	clearExclusiveRuntimeByForwardKeys([]string{}, map[string]int{})

	keys := collectForwardKeysForTest()
	assert.Empty(t, keys)
}

// TestClearAllReturnsForwardCountOnly 空缓存时全量清空返回 0（正向键数为 0）。
func TestClearAllReturnsForwardCountOnly(t *testing.T) {
	useClearAffinityTest(t)

	assert.Equal(t, 0, ClearChannelAffinityCacheAll())
}

// useRulesUpdateTest 在 useClearAffinityTest 基础上注入开关清空测试规则 r1
//（include_rule_name=true，键前缀 r1:），并对 r1 三命名空间各造一条数据
//（正向键 "r1:fp1"、occupancy 渠道 311、lastBind 同 suffix）。
func useRulesUpdateTest(t *testing.T) {
	t.Helper()
	useClearAffinityTest(t)

	setting := operation_setting.GetChannelAffinitySetting()
	originalRules := setting.Rules
	setting.Rules = append(append([]operation_setting.ChannelAffinityRule{}, setting.Rules...),
		operation_setting.ChannelAffinityRule{
			Name:            "r1",
			ModelRegex:      []string{"^gpt-.*$"},
			KeySources:      []operation_setting.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}},
			IncludeRuleName: true,
			ExclusiveBind:   false,
		})
	t.Cleanup(func() { setting.Rules = originalRules })

	ttl := 10 * time.Second
	forward := getChannelAffinityCache()
	require.NoError(t, forward.SetWithTTL("r1:fp1", 311, ttl))
	// 指纹与键末段一致（suffix "r1:fp1" 的亲和值段为 "fp1"）。
	require.NoError(t, occupancyAddKeyFP(311, affinityFingerprint("fp1"), ttl))
	require.NoError(t, lastBindSet("r1:fp1", 311, 2*ttl))
}

// assertR1RuntimeCleared 断言 r1 三处运行时数据状态：cleared 为 true 时
// 正向、occupancy、lastBind 全清；false 时全部仍在。
func assertR1RuntimeCleared(t *testing.T, cleared bool) {
	t.Helper()
	forward := getChannelAffinityCache()

	_, found, err := forward.Get("r1:fp1")
	require.NoError(t, err)
	assert.Equal(t, !cleared, found, "forward r1:fp1 cleared=%v", cleared)

	_, found, err = lastBindGet("r1:fp1")
	require.NoError(t, err)
	assert.Equal(t, !cleared, found, "lastBind r1:fp1 cleared=%v", cleared)

	count, fps, err := occupancyBindingCount(311)
	require.NoError(t, err)
	if cleared {
		assert.Equal(t, 0, count)
		assert.Empty(t, fps)
	} else {
		assert.Equal(t, 1, count)
		assert.ElementsMatch(t, []string{affinityFingerprint("fp1")}, fps)
	}
}

// TestHandleChannelAffinityRulesUpdate 计划核心断言（SSOT 4.1.4 规则1、5.2.2 第4条）：
// 例1 开关变化（false→true）触发 r1 三处全清；例2 开关未变不清空；
// 例3 旧串非法 JSON 静默返回、无 panic、无清空。
func TestHandleChannelAffinityRulesUpdate(t *testing.T) {
	tests := []struct {
		name     string
		oldRules string
		newRules string
		cleared  bool
	}{
		{
			name:     "toggle_changed",
			oldRules: `[{"name":"r1","include_rule_name":true,"exclusive_bind":false}]`,
			newRules: `[{"name":"r1","include_rule_name":true,"exclusive_bind":true}]`,
			cleared:  true,
		},
		{
			name:     "toggle_unchanged",
			oldRules: `[{"name":"r1","include_rule_name":true,"exclusive_bind":true}]`,
			newRules: `[{"name":"r1","include_rule_name":true,"exclusive_bind":true}]`,
			cleared:  false,
		},
		{
			name:     "invalid_old_json",
			oldRules: `not-a-json-array`,
			newRules: `[{"name":"r1","include_rule_name":true,"exclusive_bind":true}]`,
			cleared:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useRulesUpdateTest(t)

			require.NotPanics(t, func() {
				HandleChannelAffinityRulesUpdate(tt.oldRules, tt.newRules)
			})
			assertR1RuntimeCleared(t, tt.cleared)
		})
	}
}

// TestHandleRulesUpdateClearsNoPrefixRule 旧规则未启用 include_rule_name 且
// 键构成无规则名前缀可区分时，开关变化退化为整体清空（SysLog 声明，
// 03 文档 3.1 服务端处理逻辑第2条工程边界）：其它规则 r2 的正向数据同批清掉。
func TestHandleRulesUpdateClearsNoPrefixRule(t *testing.T) {
	useClearAffinityTest(t)

	setting := operation_setting.GetChannelAffinitySetting()
	originalRules := setting.Rules
	setting.Rules = []operation_setting.ChannelAffinityRule{
		{
			Name:            "r2",
			ModelRegex:      []string{"^gpt-.*$"},
			KeySources:      []operation_setting.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}},
			IncludeRuleName: false,
		},
	}
	t.Cleanup(func() { setting.Rules = originalRules })

	ttl := 10 * time.Second
	forward := getChannelAffinityCache()
	require.NoError(t, forward.SetWithTTL("fp1-value", 311, ttl))
	require.NoError(t, occupancyAddKeyFP(311, affinityFingerprint("fp1-value"), ttl))
	require.NoError(t, lastBindSet("fp1-value", 311, 2*ttl))
	// 另一规则前缀键，整体清空路径下一并清除。
	require.NoError(t, forward.SetWithTTL("other-rule:key-x", 322, ttl))

	HandleChannelAffinityRulesUpdate(
		`[{"name":"r2","exclusive_bind":false}]`,
		`[{"name":"r2","exclusive_bind":true}]`,
	)

	for _, suffix := range []string{"fp1-value", "other-rule:key-x"} {
		_, found, err := forward.Get(suffix)
		require.NoError(t, err)
		assert.False(t, found, "forward %s should be cleared by full clear fallback", suffix)
	}
	count, fps, err := occupancyBindingCount(311)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, fps)
	_, found, err := lastBindGet("fp1-value")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestHandleRulesUpdateInvalidNewJSON 新串非法 JSON 时同样静默返回，无清空。
func TestHandleRulesUpdateInvalidNewJSON(t *testing.T) {
	useRulesUpdateTest(t)

	require.NotPanics(t, func() {
		HandleChannelAffinityRulesUpdate(
			`[{"name":"r1","include_rule_name":true,"exclusive_bind":false}]`,
			`{invalid`,
		)
	})
	assertR1RuntimeCleared(t, false)
}

// TestHandleRulesUpdateEmptyArrays 空数组与空串输入均静默返回，无清空、无 panic。
func TestHandleRulesUpdateEmptyArrays(t *testing.T) {
	useRulesUpdateTest(t)

	require.NotPanics(t, func() {
		HandleChannelAffinityRulesUpdate("", `[{"name":"r1","include_rule_name":true,"exclusive_bind":true}]`)
		HandleChannelAffinityRulesUpdate(`[]`, `[]`)
		HandleChannelAffinityRulesUpdate(`[{"name":"r1","include_rule_name":true,"exclusive_bind":false}]`, `[]`)
	})
	assertR1RuntimeCleared(t, false)
}

// ===== T8：降级审计标记与警告日志 =====
// 覆盖 SSOT 5.3.2 第1、2条（admin_info 追加降级标记、LogWarn 警告日志）、
// 5.3.3（审计仅存指纹，非管理员视图剥离 admin_info 沿用现有机制）、
// 5.3.4 规则1（审计不改变请求响应与计费）、5.3.5（审计写入失败静默跳过）。

// buildDegradeMarkContext 构造带亲和 meta 与降级标记的 gin context：
// degrade 为 nil 时仅设 meta（无降级场景）。
func buildDegradeMarkContext(t *testing.T, degrade map[string]interface{}) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	setChannelAffinityContext(ctx, channelAffinityMeta{
		CacheKey:       channelAffinityCacheNamespace + ":r1:default:key-a",
		RuleName:       "r1",
		UsingGroup:     "default",
		ModelName:      "gpt-4",
		RequestPath:    "/v1/chat/completions",
		KeyFingerprint: "fp123456",
	})
	if degrade != nil {
		ctx.Set(ginKeyChannelAffinityExclusiveDegrade, degrade)
	}
	return ctx
}

// TestMarkChannelAffinityUsedWithDegrade 计划核心断言：gin context 设降级标记后，
// MarkChannelAffinityUsed 把 exclusive_degrade 并入 info map，经
// AppendChannelAffinityAdminInfo 挂到 adminInfo["channel_affinity"]，
// 四字段（rule_name/key_fp/channel_id/channel_binding_count）与输入深等
// （SSOT 5.3.2 第1条，04 文档 §4.3 结构）。
func TestMarkChannelAffinityUsedWithDegrade(t *testing.T) {
	degrade := map[string]interface{}{
		"rule_name":             "r1",
		"key_fp":                "fp123456",
		"channel_id":            9,
		"channel_binding_count": 3,
	}
	ctx := buildDegradeMarkContext(t, degrade)

	MarkChannelAffinityUsed(ctx, "default", 9)

	adminInfo := map[string]interface{}{}
	AppendChannelAffinityAdminInfo(ctx, adminInfo)

	affinityAny, ok := adminInfo["channel_affinity"]
	require.True(t, ok, "admin_info must carry channel_affinity")
	affinity, ok := affinityAny.(map[string]interface{})
	require.True(t, ok)
	degradeOutAny, ok := affinity["exclusive_degrade"]
	require.True(t, ok, "exclusive_degrade must be merged into info map")
	assert.Equal(t, degrade, degradeOutAny, "exclusive_degrade fields must deep-equal the marker")
}

// TestMarkChannelAffinityUsedWithoutDegrade 计划核心断言：context 无降级标记时
// info map 不含 exclusive_degrade 键（正常独占/软亲和路径无审计噪声）。
func TestMarkChannelAffinityUsedWithoutDegrade(t *testing.T) {
	ctx := buildDegradeMarkContext(t, nil)

	MarkChannelAffinityUsed(ctx, "default", 9)

	anyInfo, ok := ctx.Get(ginKeyChannelAffinityLogInfo)
	require.True(t, ok)
	info, ok := anyInfo.(map[string]interface{})
	require.True(t, ok)
	assert.NotContains(t, info, "exclusive_degrade")
}

// TestMarkChannelAffinityUsedDegradeMarkerTypeMismatch 降级标记类型不符
//（非法写入场景）时静默跳过追加，请求链路照常完成（SSOT 5.3.5 审计写入失败
// 静默跳过：map 写入无失败路径，防御类型断言失败分支）。
func TestMarkChannelAffinityUsedDegradeMarkerTypeMismatch(t *testing.T) {
	ctx := buildDegradeMarkContext(t, nil)
	ctx.Set(ginKeyChannelAffinityExclusiveDegrade, "not-a-map")

	require.NotPanics(t, func() {
		MarkChannelAffinityUsed(ctx, "default", 9)
	})

	anyInfo, ok := ctx.Get(ginKeyChannelAffinityLogInfo)
	require.True(t, ok)
	info, ok := anyInfo.(map[string]interface{})
	require.True(t, ok)
	assert.NotContains(t, info, "exclusive_degrade")
}

// TestDegradedCounter 计划核心断言：两次模拟降级计数（直接 atomic.Add）后
// GetChannelAffinityDegradedReuseTotal() 返回增量 2（SSOT 5.3.3 进程累计值，
// 供统计接口 degraded_reuse_total 读取）。
func TestDegradedCounter(t *testing.T) {
	before := GetChannelAffinityDegradedReuseTotal()
	atomic.AddUint64(&channelAffinityDegradedReuseTotal, 1)
	atomic.AddUint64(&channelAffinityDegradedReuseTotal, 1)
	assert.Equal(t, before+2, GetChannelAffinityDegradedReuseTotal())
}
