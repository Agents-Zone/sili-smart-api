package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// ChannelAffinityRule.ExclusiveBind 的 JSON 反序列化行为：
// 显式 true 落 true；旧 JSON 缺字段落零值 false（向后兼容）；
// 显式 false 落 false。
func TestChannelAffinityRuleExclusiveBindUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		json string
		want bool
	}{
		{
			name: "explicit true",
			json: `{"name":"r1","model_regex":["^a$"],"path_regex":[],"key_sources":[{"type":"gjson","path":"metadata.user_id"}],"exclusive_bind":true}`,
			want: true,
		},
		{
			name: "missing field defaults to false",
			json: `{"name":"r1","model_regex":["^a$"],"path_regex":[],"key_sources":[{"type":"gjson","path":"metadata.user_id"}]}`,
			want: false,
		},
		{
			name: "explicit false",
			json: `{"name":"r1","model_regex":["^a$"],"path_regex":[],"key_sources":[{"type":"gjson","path":"metadata.user_id"}],"exclusive_bind":false}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rule operation_setting.ChannelAffinityRule
			require.NoError(t, common.Unmarshal([]byte(tt.json), &rule))
			assert.Equal(t, tt.want, rule.ExclusiveBind)
		})
	}
}

// 默认内置规则（codex/claude cli trace）不启用独占绑定，保持零值 false。
func TestChannelAffinityBuiltinRulesExclusiveBindDefaultOff(t *testing.T) {
	setting := operation_setting.GetChannelAffinitySetting()
	require.NotEmpty(t, setting.Rules)
	for _, rule := range setting.Rules {
		assert.False(t, rule.ExclusiveBind, "builtin rule %s should default exclusive_bind to false", rule.Name)
	}
}

// resetExclusiveStoreSingletons 重置两个存储层单例。HybridCache 在构造时捕获
// common.RDB 的当前值，用例切换 Redis 全局后需重建单例才能生效（同包测试直改包级变量）。
func resetExclusiveStoreSingletons() {
	channelAffinityOccupancyOnce = sync.Once{}
	channelAffinityOccupancyCache = nil
	channelAffinityLastBindOnce = sync.Once{}
	channelAffinityLastBindCache = nil
}

// useExclusiveMemoryMode 强制两个存储层运行于内存模式（Redis 关闭），
// 并在每个用例后清空条目，避免用例间串扰。
func useExclusiveMemoryMode(t *testing.T) {
	t.Helper()
	prevEnabled := common.RedisEnabled
	prevRDB := common.RDB
	common.RedisEnabled = false
	common.RDB = nil
	resetExclusiveStoreSingletons()
	t.Cleanup(func() {
		common.RedisEnabled = prevEnabled
		common.RDB = prevRDB
		resetExclusiveStoreSingletons()
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{strconv.Itoa(101), strconv.Itoa(202), strconv.Itoa(303), strconv.Itoa(999)})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{"rule1:fp1", "rule2:fp2"})
	})
}

// TestOccupancyAddRemoveKeyFP 覆盖计划核心断言：登记、去重、移除、空集合删除条目。
func TestOccupancyAddRemoveKeyFP(t *testing.T) {
	useExclusiveMemoryMode(t)
	ttl := 10 * time.Second

	require.NoError(t, occupancyAddKeyFP(101, "a1b2c3d4", ttl))
	require.NoError(t, occupancyAddKeyFP(101, "e5f6a7b8", ttl))
	require.NoError(t, occupancyAddKeyFP(101, "a1b2c3d4", ttl)) // 重复登记去重

	count, fps, err := occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.ElementsMatch(t, []string{"a1b2c3d4", "e5f6a7b8"}, fps)

	require.NoError(t, occupancyRemoveKeyFP(101, "a1b2c3d4"))
	count, fps, err = occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{"e5f6a7b8"}, fps)

	// 移除不存在的成员：条目不变，不报错（幂等移除）。
	require.NoError(t, occupancyRemoveKeyFP(101, "not-exist"))
	count, fps, err = occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{"e5f6a7b8"}, fps)

	// 移除最后一个成员：空集合删除条目。
	require.NoError(t, occupancyRemoveKeyFP(101, "e5f6a7b8"))
	count, fps, err = occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, fps)

	_, found, err := getChannelAffinityOccupancyCache().Get("101")
	require.NoError(t, err)
	assert.False(t, found, "occupancy entry should be deleted when key set becomes empty")
}

// TestOccupancyRemoveOnMissingEntry 对不存在的渠道条目执行移除，应无副作用且不报错。
func TestOccupancyRemoveOnMissingEntry(t *testing.T) {
	useExclusiveMemoryMode(t)

	require.NoError(t, occupancyRemoveKeyFP(999, "a1b2c3d4"))
	count, fps, err := occupancyBindingCount(999)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, fps)
}

// TestOccupancyChannelsAreIndependent 不同渠道条目互不影响（索引以渠道为聚合维度）。
func TestOccupancyChannelsAreIndependent(t *testing.T) {
	useExclusiveMemoryMode(t)
	ttl := 10 * time.Second

	require.NoError(t, occupancyAddKeyFP(101, "a1b2c3d4", ttl))
	require.NoError(t, occupancyAddKeyFP(202, "a1b2c3d4", ttl))

	count101, _, err := occupancyBindingCount(101)
	require.NoError(t, err)
	count202, _, err := occupancyBindingCount(202)
	require.NoError(t, err)
	assert.Equal(t, 1, count101)
	assert.Equal(t, 1, count202)

	require.NoError(t, occupancyRemoveKeyFP(101, "a1b2c3d4"))
	count101, _, err = occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 0, count101)

	count202, _, err = occupancyBindingCount(202)
	require.NoError(t, err)
	assert.Equal(t, 1, count202)
}

// TestOccupancyIndexKeyIsChannelIDDecimal 索引键为渠道 ID 十进制字符串（04 文档 §4.1），
// 命名空间为 new-api:channel_affinity_occupancy:v1。
func TestOccupancyIndexKeyIsChannelIDDecimal(t *testing.T) {
	useExclusiveMemoryMode(t)
	ttl := 10 * time.Second

	require.NoError(t, occupancyAddKeyFP(303, "aa11bb22", ttl))

	cache := getChannelAffinityOccupancyCache()
	entry, found, err := cache.Get("303")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []string{"aa11bb22"}, entry.KeyFPs)
	assert.Equal(t, channelAffinityOccupancyNamespace+":303", cache.FullKey("303"))
}

// TestOccupancyEntryTTLRenewedOnReAdd 登记续期整条目 TTL：先短 TTL 登记，
// 再长 TTL 重复登记同一键，条目存活期应按最近一次登记续期（04 文档 §4.1）。
func TestOccupancyEntryTTLRenewedOnReAdd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ttl renewal test in short mode")
	}
	useExclusiveMemoryMode(t)

	require.NoError(t, occupancyAddKeyFP(101, "a1b2c3d4", 150*time.Millisecond))
	time.Sleep(100 * time.Millisecond)
	// 最近一次登记以更长 TTL 续期整条目。
	require.NoError(t, occupancyAddKeyFP(101, "a1b2c3d4", 2*time.Second))
	time.Sleep(100 * time.Millisecond)

	count, _, err := occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "entry should survive via TTL renewal from latest registration")
}

// TestOccupancyRemoveDoesNotExtendTTL 移除成员写回时不续期（TTL 保持原值）。
// 用短 TTL 条目验证：登记→短暂等待→移除另一成员，原成员应随原 TTL 过期而非被续期。
func TestOccupancyRemoveDoesNotExtendTTL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ttl test in short mode")
	}
	useExclusiveMemoryMode(t)

	require.NoError(t, occupancyAddKeyFP(101, "a1b2c3d4", 200*time.Millisecond))
	require.NoError(t, occupancyAddKeyFP(101, "e5f6a7b8", 200*time.Millisecond))
	time.Sleep(120 * time.Millisecond)
	require.NoError(t, occupancyRemoveKeyFP(101, "a1b2c3d4"))
	time.Sleep(120 * time.Millisecond)

	count, _, err := occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "remove should not extend entry TTL")
}

// TestOccupancyAddKeyFPAtomicMemoryMode 内存模式下原子登记回退到 occupancyAddKeyFP，
// 行为与普通登记一致。
func TestOccupancyAddKeyFPAtomicMemoryMode(t *testing.T) {
	useExclusiveMemoryMode(t)
	ttl := 10 * time.Second

	require.NoError(t, occupancyAddKeyFPAtomic(101, "a1b2c3d4", ttl))
	require.NoError(t, occupancyAddKeyFPAtomic(101, "a1b2c3d4", ttl))

	count, fps, err := occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{"a1b2c3d4"}, fps)
}

// TestOccupancyAddScriptRegistered Lua 脚本常量已注册（Redis 模式原子登记用）。
func TestOccupancyAddScriptRegistered(t *testing.T) {
	require.NotNil(t, occupancyAddScript)
}

// TestLastBindRoundTrip 覆盖计划核心断言：set/get/delete 往返。
func TestLastBindRoundTrip(t *testing.T) {
	useExclusiveMemoryMode(t)
	ttl := 10 * time.Second

	before := common.GetTimestamp()
	require.NoError(t, lastBindSet("rule1:fp1", 42, 2*ttl))

	record, found, err := lastBindGet("rule1:fp1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 42, record.ChannelID)
	assert.GreaterOrEqual(t, record.BoundAt, before)
	assert.Greater(t, record.BoundAt, int64(0))

	require.NoError(t, lastBindDelete("rule1:fp1"))
	_, found, err = lastBindGet("rule1:fp1")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestLastBindGetMissing 键不存在时返回零值与 found=false。
func TestLastBindGetMissing(t *testing.T) {
	useExclusiveMemoryMode(t)

	record, found, err := lastBindGet("rule-missing:key")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, channelAffinityLastBindRecord{}, record)
}

// TestLastBindOverwrite 同键重复写入覆盖旧记录（迁移时更新为新渠道，SSOT 5.2.2 第2条）。
func TestLastBindOverwrite(t *testing.T) {
	useExclusiveMemoryMode(t)
	ttl := 10 * time.Second

	require.NoError(t, lastBindSet("rule2:fp2", 42, ttl))
	require.NoError(t, lastBindSet("rule2:fp2", 77, ttl))

	record, found, err := lastBindGet("rule2:fp2")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 77, record.ChannelID)
}

// TestLastBindKeysIndependentOfOccupancy 最近绑定记录键与正向缓存键一致（同一 suffix），
// 且与占用索引命名空间隔离。
func TestLastBindKeysIndependentOfOccupancy(t *testing.T) {
	useExclusiveMemoryMode(t)
	ttl := 10 * time.Second

	require.NoError(t, lastBindSet("rule1:fp1", 42, ttl))

	cache := getChannelAffinityLastBindCache()
	assert.Equal(t, channelAffinityLastBindNamespace+":rule1:fp1", cache.FullKey("rule1:fp1"))
	assert.NotEqual(t, channelAffinityOccupancyNamespace, channelAffinityLastBindNamespace)
}

// TestLastBindDeleteMissing 删除不存在的键不报错（幂等删除）。
func TestLastBindDeleteMissing(t *testing.T) {
	useExclusiveMemoryMode(t)

	require.NoError(t, lastBindDelete("rule-missing:key"))
}

// TestDecideExclusiveBinding 覆盖计划核心断言（表驱动，4 例）：
// 有空闲+绑回、有空闲无记录、满载降级、空候选。
func TestDecideExclusiveBinding(t *testing.T) {
	firstPick := func(ids []int) int {
		require.NotEmpty(t, ids)
		return ids[0]
	}

	tests := []struct {
		name              string
		candidates        []int
		occupancyCounts   map[int]int
		lastBindChannelID int
		lastBindValid     bool
		pickWeighted      func([]int) int
		want              channelAffinityBindDecision
	}{
		{
			name:              "free_channel_rebinds_last_bind",
			candidates:        []int{1, 2, 3},
			occupancyCounts:   map[int]int{1: 0, 2: 1, 3: 0},
			lastBindChannelID: 3,
			lastBindValid:     true,
			pickWeighted:      firstPick,
			want:              channelAffinityBindDecision{ChannelID: 3, Mode: affinityBindModeExclusive},
		},
		{
			name:            "free_channel_no_record_picks_weighted",
			candidates:      []int{1, 2},
			occupancyCounts: map[int]int{1: 1, 2: 0},
			lastBindValid:   false,
			pickWeighted:    firstPick,
			want:            channelAffinityBindDecision{ChannelID: 2, Mode: affinityBindModeExclusive},
		},
		{
			name:            "full_load_picks_fewest_shared",
			candidates:      []int{1, 2, 3},
			occupancyCounts: map[int]int{1: 3, 2: 1, 3: 2},
			lastBindValid:   false,
			pickWeighted:    firstPick,
			want:            channelAffinityBindDecision{ChannelID: 2, Mode: affinityBindModeShared},
		},
		{
			name:         "empty_candidates",
			candidates:   []int{},
			pickWeighted: firstPick,
			want:         channelAffinityBindDecision{ChannelID: 0, Mode: ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideExclusiveBinding(tt.candidates, tt.occupancyCounts, tt.lastBindChannelID, tt.lastBindValid, tt.pickWeighted)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestDecideExclusiveBinding_RebindChannelNotFree lastBind 记录的渠道已被其它键占用
// （不在空闲集）时，忽略记录走正常独占选路（SSOT 5.1.4 规则4）。
func TestDecideExclusiveBinding_RebindChannelNotFree(t *testing.T) {
	got := decideExclusiveBinding(
		[]int{1, 2},
		map[int]int{1: 1, 2: 0},
		1, true,
		func(ids []int) int { return ids[0] },
	)
	assert.Equal(t, channelAffinityBindDecision{ChannelID: 2, Mode: affinityBindModeExclusive}, got)
}

// TestDecideExclusiveBinding_RebindChannelNotCandidate lastBind 记录的渠道已不在候选集
// （渠道不可用）时，走正常独占选路（SSOT 5.1.4 规则4）。
func TestDecideExclusiveBinding_RebindChannelNotCandidate(t *testing.T) {
	got := decideExclusiveBinding(
		[]int{1, 2},
		map[int]int{1: 0, 2: 0},
		99, true,
		func(ids []int) int { return ids[0] },
	)
	assert.Equal(t, channelAffinityBindDecision{ChannelID: 1, Mode: affinityBindModeExclusive}, got)
}

// TestDecideExclusiveBinding_FullLoadTieBrokenByPick 满载时最少绑定数并列，
// pickWeighted 在同层内选定（SSOT 5.1.4 规则3）。
func TestDecideExclusiveBinding_FullLoadTieBrokenByPick(t *testing.T) {
	got := decideExclusiveBinding(
		[]int{1, 2, 3},
		map[int]int{1: 2, 2: 1, 3: 1},
		0, false,
		func(ids []int) int {
			require.Equal(t, []int{2, 3}, ids, "pickWeighted should receive the fewest-binding tier only")
			return 3
		},
	)
	assert.Equal(t, channelAffinityBindDecision{ChannelID: 3, Mode: affinityBindModeShared}, got)
}

// TestDecideExclusiveBinding_OwnBindingTreatedAsFree 本键既有绑定视为空闲：
// 调用方已把本键登记的渠道键数减 1 后传入（SSOT 5.1.2 流程第2条），
// occupancyCounts[id]==0 即空闲，决策函数不区分来源。
func TestDecideExclusiveBinding_OwnBindingTreatedAsFree(t *testing.T) {
	// 渠道 1 索引键数为 1，但那是本键自己的登记，调用方传入 0：空闲，绑回。
	got := decideExclusiveBinding(
		[]int{1},
		map[int]int{1: 0},
		1, true,
		func(ids []int) int { return ids[0] },
	)
	assert.Equal(t, channelAffinityBindDecision{ChannelID: 1, Mode: affinityBindModeExclusive}, got)
}

// TestDecideExclusiveBinding_FullLoadAllOccupied 独占判定以渠道为单位全局生效：
// occupancyCounts 覆盖全部键，候选渠道全部被占用（满载）即降级复用，
// 即便只有一个候选渠道也走 shared（SSOT 5.1.4 规则1+规则3）。
func TestDecideExclusiveBinding_FullLoadAllOccupied(t *testing.T) {
	got := decideExclusiveBinding(
		[]int{1},
		map[int]int{1: 5},
		1, true, // 记录渠道被占用，空闲集为空
		func(ids []int) int { return ids[0] },
	)
	assert.Equal(t, channelAffinityBindDecision{ChannelID: 1, Mode: affinityBindModeShared}, got)
}

// TestAffinityBindModeConstants Mode 常量取值与计划契约一致。
func TestAffinityBindModeConstants(t *testing.T) {
	assert.Equal(t, "exclusive", affinityBindModeExclusive)
	assert.Equal(t, "shared", affinityBindModeShared)
}

// useExclusiveRedisMode 将存储层切到 miniredis（Redis 模式），返回 server 供键断言。
func useExclusiveRedisMode(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	prevEnabled := common.RedisEnabled
	prevRDB := common.RDB
	common.RedisEnabled = true
	common.RDB = client
	resetExclusiveStoreSingletons()
	t.Cleanup(func() {
		_ = client.Close()
		common.RedisEnabled = prevEnabled
		common.RDB = prevRDB
		resetExclusiveStoreSingletons()
	})
	return server
}

// TestOccupancyRedisModeAddAndCount Redis 模式下登记/去重/计数经 JSONCodec 条目读写。
func TestOccupancyRedisModeAddAndCount(t *testing.T) {
	server := useExclusiveRedisMode(t)
	ttl := 10 * time.Second

	require.NoError(t, occupancyAddKeyFP(101, "a1b2c3d4", ttl))
	require.NoError(t, occupancyAddKeyFP(101, "e5f6a7b8", ttl))
	require.NoError(t, occupancyAddKeyFP(101, "a1b2c3d4", ttl))

	count, fps, err := occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.ElementsMatch(t, []string{"a1b2c3d4", "e5f6a7b8"}, fps)
	assert.True(t, server.Exists(channelAffinityOccupancyNamespace+":101"))

	require.NoError(t, occupancyRemoveKeyFP(101, "a1b2c3d4"))
	count, fps, err = occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{"e5f6a7b8"}, fps)

	require.NoError(t, occupancyRemoveKeyFP(101, "e5f6a7b8"))
	count, _, err = occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.False(t, server.Exists(channelAffinityOccupancyNamespace+":101"), "empty entry should be deleted")
}

// TestOccupancyAddKeyFPAtomicRedisMode Redis 模式下原子登记走 Lua 脚本：
// 登记两个不同指纹后条目含两成员；重复指纹不重复追加。
func TestOccupancyAddKeyFPAtomicRedisMode(t *testing.T) {
	server := useExclusiveRedisMode(t)
	ttl := 10 * time.Second

	require.NoError(t, occupancyAddKeyFPAtomic(101, "a1b2c3d4", ttl))
	require.NoError(t, occupancyAddKeyFPAtomic(101, "e5f6a7b8", ttl))
	require.NoError(t, occupancyAddKeyFPAtomic(101, "a1b2c3d4", ttl))

	count, fps, err := occupancyBindingCount(101)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.ElementsMatch(t, []string{"a1b2c3d4", "e5f6a7b8"}, fps)
	assert.True(t, server.Exists(channelAffinityOccupancyNamespace+":101"))
}

// TestLastBindRedisRoundTrip Redis 模式下最近绑定记录写入/读取/删除往返。
func TestLastBindRedisRoundTrip(t *testing.T) {
	useExclusiveRedisMode(t)
	ttl := 10 * time.Second

	require.NoError(t, lastBindSet("rule1:fp1", 42, 2*ttl))

	record, found, err := lastBindGet("rule1:fp1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 42, record.ChannelID)
	assert.Greater(t, record.BoundAt, int64(0))

	require.NoError(t, lastBindDelete("rule1:fp1"))
	_, found, err = lastBindGet("rule1:fp1")
	require.NoError(t, err)
	assert.False(t, found)
}

// resetAffinityCacheSingleton 重置正向亲和缓存单例（与 exclusive 存储层同模式），
// 供用例切换 Redis 全局后重建。
func resetAffinityCacheSingleton() {
	channelAffinityCacheOnce = sync.Once{}
	channelAffinityCache = nil
}

// setupExclusiveAcquireDB 建内存库表并注入独占测试渠道（701、702 属 default 分组，
// gpt-4 模型）。内存缓存启用走 group2model2channels 候选分支（service 包 TestMain
// 未跑 initCol，DB 分支的 group 列名未初始化，故按现有 service 选路用例的模式
// 用内存缓存）。
func setupExclusiveAcquireDB(t *testing.T) {
	t.Helper()

	originalDB := model.DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))

	priority := int64(0)
	weight := uint(100)
	for _, id := range []int{701, 702} {
		require.NoError(t, db.Create(&model.Channel{
			Id:       id,
			Type:     constant.ChannelTypeOpenAI,
			Key:      fmt.Sprintf("key-%d", id),
			Status:   common.ChannelStatusEnabled,
			Name:     fmt.Sprintf("exclusive-channel-%d", id),
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

// buildExclusiveAcquireContext 构造带亲和 meta 的 gin context。
func buildExclusiveAcquireContext(t *testing.T, meta channelAffinityMeta) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	setChannelAffinityContext(ctx, meta)
	return ctx
}

// exclusiveAcquireTestMeta 构造并发用例的 meta（候选集 [701,702]）。
func exclusiveAcquireTestMeta() channelAffinityMeta {
	return channelAffinityMeta{
		CacheKey:       channelAffinityCacheNamespace + ":exclusive-test-rule:default:key-a",
		TTLSeconds:     30,
		RuleName:       "exclusive-test-rule",
		ExclusiveBind:  true,
		KeyFingerprint: "aaaa1111",
		UsingGroup:     "default",
		ModelName:      "gpt-4",
		RequestPath:    "/v1/chat/completions",
	}
}

// cleanupExclusiveAcquire 清理占位三处残留。
func cleanupExclusiveAcquire(meta channelAffinityMeta) func() {
	return func() {
		_, _ = getChannelAffinityCache().DeleteMany([]string{meta.CacheKey})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"701", "702"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	}
}

// exclusiveCacheKeySuffix 由 meta.CacheKey 去正向前缀得到 suffix。
func exclusiveCacheKeySuffix(meta channelAffinityMeta) string {
	return strings.TrimPrefix(meta.CacheKey, channelAffinityCacheNamespace+":")
}

// TestAcquireExclusiveBinding_ConcurrentFirstWins 计划核心断言：两个 goroutine 并发
// 对同一 cacheKeySuffix（同 meta、同候选集 [701,702]）调 acquireExclusiveBinding，
// 汇合后两者返回同一 channelID（701 或 702 之一），且该渠道占用键数为 1
// （仅胜者指纹登记一次）（SSOT 5.1.2 流程第2条末）。
func TestAcquireExclusiveBinding_ConcurrentFirstWins(t *testing.T) {
	useExclusiveMemoryMode(t)
	setupExclusiveAcquireDB(t)
	meta := exclusiveAcquireTestMeta()
	t.Cleanup(cleanupExclusiveAcquire(meta))
	resetAffinityCacheSingleton()

	const goroutines = 2
	results := make([]int, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			channelID, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
			require.True(t, found)
			require.Contains(t, []int{701, 702}, channelID)
			results[idx] = channelID
		}(i)
	}
	close(start)
	wg.Wait()

	assert.Equal(t, results[0], results[1], "concurrent first bind must converge to the winner channel")

	count, fps, err := occupancyBindingCount(results[0])
	require.NoError(t, err)
	assert.Equal(t, 1, count, "only the winner fingerprint should be registered once")
	assert.ElementsMatch(t, []string{meta.KeyFingerprint}, fps)

	// 败者读胜者结果：正向缓存与最近绑定记录均指向胜者渠道。
	cachedID, found, err := getChannelAffinityCache().Get(exclusiveCacheKeySuffix(meta))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, results[0], cachedID)

	record, found, err := lastBindGet(exclusiveCacheKeySuffix(meta))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, results[0], record.ChannelID)
}

// TestAcquireExclusiveBinding_IndexErrorDegrades 计划核心断言：键指纹为空串属提取异常，
// 守卫返回 (0, false) 直接降级软亲和，不阻塞请求且不写入任何占位（SSOT 5.1.5）。
func TestAcquireExclusiveBinding_IndexErrorDegrades(t *testing.T) {
	useExclusiveMemoryMode(t)
	setupExclusiveAcquireDB(t)
	meta := exclusiveAcquireTestMeta()
	meta.KeyFingerprint = ""
	t.Cleanup(cleanupExclusiveAcquire(meta))
	resetAffinityCacheSingleton()

	channelID, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	assert.False(t, found)
	assert.Equal(t, 0, channelID)

	// 未写入任何占位：正向缓存、占用索引、最近绑定记录三处均空。
	_, found, err := getChannelAffinityCache().Get(exclusiveCacheKeySuffix(meta))
	require.NoError(t, err)
	assert.False(t, found)

	for _, id := range []int{701, 702} {
		count, _, err := occupancyBindingCount(id)
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	}

	_, found, err = lastBindGet(exclusiveCacheKeySuffix(meta))
	require.NoError(t, err)
	assert.False(t, found)
}

// TestAcquireExclusiveBinding_EmptyCandidatesNoBind 候选集为空时无可绑定渠道，
// 返回 (0, false) 且不写占位（分组无可用渠道场景）。
func TestAcquireExclusiveBinding_EmptyCandidatesNoBind(t *testing.T) {
	useExclusiveMemoryMode(t)
	setupExclusiveAcquireDB(t)
	meta := exclusiveAcquireTestMeta()
	meta.UsingGroup = "no-such-group"
	t.Cleanup(cleanupExclusiveAcquire(meta))
	resetAffinityCacheSingleton()

	channelID, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	assert.False(t, found)
	assert.Equal(t, 0, channelID)

	_, found, err := lastBindGet(exclusiveCacheKeySuffix(meta))
	require.NoError(t, err)
	assert.False(t, found)
}

// TestAcquireExclusiveBinding_ExcludesOtherKeyOccupancy 独占判定全局生效：另一键已占
// 701 时，本键选 702（SSOT 5.1.4 规则1，经 occupancyCounts 覆盖全部键实现）。
func TestAcquireExclusiveBinding_ExcludesOtherKeyOccupancy(t *testing.T) {
	useExclusiveMemoryMode(t)
	setupExclusiveAcquireDB(t)
	meta := exclusiveAcquireTestMeta()
	t.Cleanup(cleanupExclusiveAcquire(meta))
	resetAffinityCacheSingleton()

	require.NoError(t, occupancyAddKeyFP(701, "other1", 10*time.Second))

	channelID, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	assert.Equal(t, 702, channelID)

	count, fps, err := occupancyBindingCount(702)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{meta.KeyFingerprint}, fps)
}

// TestAcquireExclusiveBinding_FullLoadSharedDegrades 满载降级：两个渠道均被其它键占用，
// 按最少绑定数优先复用（701 键数 1、702 键数 2，选 701），Mode=shared 触发降级计数与
// gin context 降级标记（04 文档 §4.3 结构）。
func TestAcquireExclusiveBinding_FullLoadSharedDegrades(t *testing.T) {
	useExclusiveMemoryMode(t)
	setupExclusiveAcquireDB(t)
	meta := exclusiveAcquireTestMeta()
	t.Cleanup(cleanupExclusiveAcquire(meta))
	resetAffinityCacheSingleton()

	require.NoError(t, occupancyAddKeyFP(701, "other1", 10*time.Second))
	require.NoError(t, occupancyAddKeyFP(702, "other1", 10*time.Second))
	require.NoError(t, occupancyAddKeyFP(702, "other2", 10*time.Second))

	degradedBefore := atomic.LoadUint64(&channelAffinityDegradedReuseTotal)

	ctx := buildExclusiveAcquireContext(t, meta)
	channelID, found := acquireExclusiveBinding(ctx, meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	assert.Equal(t, 701, channelID, "full load should reuse the fewest-binding channel")

	assert.Equal(t, degradedBefore+1, atomic.LoadUint64(&channelAffinityDegradedReuseTotal), "shared mode must bump degraded counter once")

	anyDegrade, ok := ctx.Get(ginKeyChannelAffinityExclusiveDegrade)
	require.True(t, ok, "shared mode must write degrade marker into gin context")
	degrade, ok := anyDegrade.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, meta.RuleName, degrade["rule_name"])
	assert.Equal(t, meta.KeyFingerprint, degrade["key_fp"])
	assert.Equal(t, 701, degrade["channel_id"])
	assert.Equal(t, 1, degrade["channel_binding_count"], "channel_binding_count is the count at decision time")
}

// TestAcquireExclusiveBinding_RebindsLastBindChannel TTL 到期重绑优先绑回原渠道：
// lastBind 记录 702 且 702 空闲，选 702（SSOT 5.1.4 规则4）。
func TestAcquireExclusiveBinding_RebindsLastBindChannel(t *testing.T) {
	useExclusiveMemoryMode(t)
	setupExclusiveAcquireDB(t)
	meta := exclusiveAcquireTestMeta()
	t.Cleanup(cleanupExclusiveAcquire(meta))
	resetAffinityCacheSingleton()

	require.NoError(t, lastBindSet(exclusiveCacheKeySuffix(meta), 702, 20*time.Second))

	channelID, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	assert.Equal(t, 702, channelID, "rebind should prefer the last-bound channel when free")
}

// TestAcquireExclusiveBinding_ExistingWinnerShortCircuits 锁内 double-check：正向缓存
// 已有胜者写入时（并发败者路径），直接返回胜者渠道，不重复登记指纹。
func TestAcquireExclusiveBinding_ExistingWinnerShortCircuits(t *testing.T) {
	useExclusiveMemoryMode(t)
	setupExclusiveAcquireDB(t)
	meta := exclusiveAcquireTestMeta()
	t.Cleanup(cleanupExclusiveAcquire(meta))
	resetAffinityCacheSingleton()

	// 预置胜者占位：正向绑定 701 + 索引登记。
	require.NoError(t, getChannelAffinityCache().SetWithTTL(exclusiveCacheKeySuffix(meta), 701, 10*time.Second))
	require.NoError(t, occupancyAddKeyFP(701, "winner1", 10*time.Second))

	channelID, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	assert.Equal(t, 701, channelID)

	// 索引仍只含胜者指纹一次，本键指纹未追加。
	count, fps, err := occupancyBindingCount(701)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{"winner1"}, fps)
}

// TestAcquireExclusiveBinding_AutoGroupExpandsCandidates usingGroup=auto 时候选集
// 经 GetRequestAutoGroups 展开为多分组并集。
func TestAcquireExclusiveBinding_AutoGroupExpandsCandidates(t *testing.T) {
	useExclusiveMemoryMode(t)

	originalDB := model.DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))

	priority := int64(0)
	weight := uint(100)
	// vip 分组 703、default 分组 704；703 被占后应经 auto 展开把 704 纳入候选。
	for _, tc := range []struct {
		id    int
		group string
	}{{703, "vip"}, {704, "default"}} {
		require.NoError(t, db.Create(&model.Channel{
			Id:       tc.id,
			Type:     constant.ChannelTypeOpenAI,
			Key:      fmt.Sprintf("key-%d", tc.id),
			Status:   common.ChannelStatusEnabled,
			Name:     fmt.Sprintf("auto-channel-%d", tc.id),
			Weight:   &weight,
			Models:   "gpt-4",
			Group:    tc.group,
			Priority: &priority,
		}).Error)
		require.NoError(t, db.Create(&model.Ability{
			Group:     tc.group,
			Model:     "gpt-4",
			ChannelId: tc.id,
			Enabled:   true,
			Priority:  &priority,
			Weight:    weight,
		}).Error)
	}
	model.DB = db
	common.MemoryCacheEnabled = true
	model.InitChannelCache()

	meta := exclusiveAcquireTestMeta()
	meta.UsingGroup = "auto"
	t.Cleanup(func() {
		model.DB = originalDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		if originalMemoryCacheEnabled && originalDB != nil {
			model.InitChannelCache()
		}
		_, _ = getChannelAffinityCache().DeleteMany([]string{meta.CacheKey})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"703", "704"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	resetAffinityCacheSingleton()

	ctx := buildExclusiveAcquireContext(t, meta)
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyTokenAutoGroups, []string{"vip", "default"})

	// 先占满 vip 分组的 703，auto 展开后应把 default 分组的 704 也纳入候选并选中。
	require.NoError(t, occupancyAddKeyFP(703, "other1", 10*time.Second))

	channelID, found := acquireExclusiveBinding(ctx, meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	assert.Equal(t, 704, channelID)
}

// setupRecordAffinityTest 构造 RecordChannelAffinity 用例的公共夹具：
// 内存模式存储 + 默认设置（Enabled/SwitchOnSuccess 由默认内置设置提供 true）。
// 返回 gin context 与去前缀 suffix。cleanupChannelIDs 供用例结束清理占用条目。
func setupRecordAffinityTest(t *testing.T, channelIDs ...int) (*gin.Context, string) {
	t.Helper()
	useExclusiveMemoryMode(t)
	resetAffinityCacheSingleton()

	setting := operation_setting.GetChannelAffinitySetting()
	originalEnabled := setting.Enabled
	originalSwitch := setting.SwitchOnSuccess
	originalDefaultTTL := setting.DefaultTTLSeconds
	setting.Enabled = true
	setting.SwitchOnSuccess = true
	allChannels := append([]int{501, 502, 601, 602}, channelIDs...)
	t.Cleanup(func() {
		setting.Enabled = originalEnabled
		setting.SwitchOnSuccess = originalSwitch
		setting.DefaultTTLSeconds = originalDefaultTTL
		resetAffinityCacheSingleton()
		keys := make([]string, 0, len(allChannels))
		for _, id := range allChannels {
			keys = append(keys, strconv.Itoa(id))
		}
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(keys)
	})

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return ctx, ""
}

// recordAffinityMeta 构造登记用 meta（KeyFingerprint 与 CacheKey 由参数给出）。
func recordAffinityMeta(keyFP string, suffix string) channelAffinityMeta {
	return channelAffinityMeta{
		CacheKey:       channelAffinityCacheNamespace + ":" + suffix,
		TTLSeconds:     60,
		RuleName:       "record-affinity-rule",
		KeyFingerprint: keyFP,
		UsingGroup:     "default",
		ModelName:      "gpt-4",
	}
}

// buildRecordAffinityContext 构造带 meta 的 gin context 并写入首次占位渠道标记。
func buildRecordAffinityContext(t *testing.T, meta channelAffinityMeta, boundChannel int) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	setChannelAffinityContext(ctx, meta)
	if boundChannel > 0 {
		ctx.Set(ginKeyChannelAffinityBoundChannel, boundChannel)
	}
	return ctx
}

// cleanupForwardAndLastBind 清理正向缓存与最近绑定记录残留。
func cleanupForwardAndLastBind(t *testing.T, suffixes ...string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = getChannelAffinityCache().DeleteMany(suffixes)
		_, _ = getChannelAffinityLastBindCache().DeleteMany(suffixes)
	})
}

// TestRecordChannelAffinityRegistersIndexes 计划核心断言（SSOT 5.2.2 第1条）：
// RecordChannelAffinity(ctx, 501) 后 occupancyBindingCount(501) 键数 1 且含指纹，
// lastBindGet(suffix) 指向 501、BoundAt>0。
func TestRecordChannelAffinityRegistersIndexes(t *testing.T) {
	suffix := fmt.Sprintf("record-rule:default:fp-%d", time.Now().UnixNano())
	meta := recordAffinityMeta("fp1a2b3c4", suffix)
	ctx := buildRecordAffinityContext(t, meta, 501)
	setupRecordAffinityTest(t)
	cleanupForwardAndLastBind(t, suffix)

	RecordChannelAffinity(ctx, 501)

	count, fps, err := occupancyBindingCount(501)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{"fp1a2b3c4"}, fps)

	record, found, err := lastBindGet(suffix)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 501, record.ChannelID)
	assert.Greater(t, record.BoundAt, int64(0))
}

// TestRecordChannelAffinityMigratesIndexes 计划核心断言（SSOT 5.2.2 第2条）：
// SwitchOnSuccess 开启、context channel_id=502 且首次占位渠道=501，
// RecordChannelAffinity 后旧渠道 501 索引键数 0、新渠道 502 键数 1、lastBind 指向 502。
func TestRecordChannelAffinityMigratesIndexes(t *testing.T) {
	suffix := fmt.Sprintf("migrate-rule:default:fp-%d", time.Now().UnixNano())
	meta := recordAffinityMeta("fp1a2b3c4", suffix)
	ctx := buildRecordAffinityContext(t, meta, 501)
	ctx.Set("channel_id", 502)
	setupRecordAffinityTest(t)
	cleanupForwardAndLastBind(t, suffix)

	// 预置旧渠道索引：模拟首次占位已登记 501。
	require.NoError(t, occupancyAddKeyFP(501, "fp1a2b3c4", 10*time.Second))

	RecordChannelAffinity(ctx, 502)

	count501, _, err := occupancyBindingCount(501)
	require.NoError(t, err)
	assert.Equal(t, 0, count501, "old channel index must be removed on migration")

	count502, fps, err := occupancyBindingCount(502)
	require.NoError(t, err)
	assert.Equal(t, 1, count502)
	assert.ElementsMatch(t, []string{"fp1a2b3c4"}, fps)

	record, found, err := lastBindGet(suffix)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 502, record.ChannelID)
}

// TestRecordChannelAffinityMigrateWithoutPriorIndex 迁移场景但旧渠道无既有登记
// （首次占位记录值为 0 或索引已被回滚）：登记到新渠道，不因旧条目缺失而失败。
func TestRecordChannelAffinityMigrateWithoutPriorIndex(t *testing.T) {
	suffix := fmt.Sprintf("migrate-no-index:default:fp-%d", time.Now().UnixNano())
	meta := recordAffinityMeta("migrate123", suffix)
	ctx := buildRecordAffinityContext(t, meta, 0)
	ctx.Set("channel_id", 502)
	setupRecordAffinityTest(t)
	cleanupForwardAndLastBind(t, suffix)

	RecordChannelAffinity(ctx, 502)

	count502, fps, err := occupancyBindingCount(502)
	require.NoError(t, err)
	assert.Equal(t, 1, count502)
	assert.ElementsMatch(t, []string{"migrate123"}, fps)

	count501, _, err := occupancyBindingCount(501)
	require.NoError(t, err)
	assert.Equal(t, 0, count501)
}

// TestRollbackBindingPlacement 计划核心断言（SSOT 5.1.2 第4条末）：
// 三处占位登记后调 rollbackBindingPlacement，正向/反向/最近绑定全清。
func TestRollbackBindingPlacement(t *testing.T) {
	suffix := fmt.Sprintf("rollback-rule:default:fp-%d", time.Now().UnixNano())
	cacheKeyFull := channelAffinityCacheNamespace + ":" + suffix
	setupRecordAffinityTest(t)
	cleanupForwardAndLastBind(t, suffix)

	require.NoError(t, getChannelAffinityCache().SetWithTTL(suffix, 601, 10*time.Second))
	require.NoError(t, occupancyAddKeyFP(601, "fp1a2b3c4", 10*time.Second))
	require.NoError(t, lastBindSet(suffix, 601, 20*time.Second))

	rollbackBindingPlacement(cacheKeyFull, "fp1a2b3c4", 601)

	_, found, err := getChannelAffinityCache().Get(suffix)
	require.NoError(t, err)
	assert.False(t, found, "forward binding must be deleted")

	count, _, err := occupancyBindingCount(601)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "occupancy index must be cleared")

	_, found, err = lastBindGet(suffix)
	require.NoError(t, err)
	assert.False(t, found, "last bind record must be deleted")
}

// TestRollbackOnFinalFailure_NoSwitch 计划核心断言：占位渠道 601、context channel_id=601
// （未切换），终态失败回滚后三处全清（SSOT 5.1.2 第4条末、5.1.5）。
func TestRollbackOnFinalFailure_NoSwitch(t *testing.T) {
	suffix := fmt.Sprintf("final-no-switch:default:fp-%d", time.Now().UnixNano())
	meta := recordAffinityMeta("fp1a2b3c4", suffix)
	ctx := buildRecordAffinityContext(t, meta, 601)
	ctx.Set("channel_id", 601)
	setupRecordAffinityTest(t)
	cleanupForwardAndLastBind(t, suffix)

	require.NoError(t, getChannelAffinityCache().SetWithTTL(suffix, 601, 10*time.Second))
	require.NoError(t, occupancyAddKeyFP(601, "fp1a2b3c4", 10*time.Second))
	require.NoError(t, lastBindSet(suffix, 601, 20*time.Second))

	RollbackChannelAffinityOnFinalFailure(ctx)

	_, found, err := getChannelAffinityCache().Get(suffix)
	require.NoError(t, err)
	assert.False(t, found)

	count, _, err := occupancyBindingCount(601)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	_, found, err = lastBindGet(suffix)
	require.NoError(t, err)
	assert.False(t, found)
}

// TestRollbackOnFinalFailure_RetrySwitchedAlsoRollsBack 计划核心断言：占位渠道 601、
// 重试切换到 602 后仍终态失败（context channel_id=602），三处占位全清——终态失败
// 出口必然未发生成功切换，失败重试切换同样回滚（SSOT 5.1.2 第4条末）。
func TestRollbackOnFinalFailure_RetrySwitchedAlsoRollsBack(t *testing.T) {
	suffix := fmt.Sprintf("final-switched:default:fp-%d", time.Now().UnixNano())
	meta := recordAffinityMeta("fp1a2b3c4", suffix)
	ctx := buildRecordAffinityContext(t, meta, 601)
	ctx.Set("channel_id", 602)
	setupRecordAffinityTest(t)
	cleanupForwardAndLastBind(t, suffix)

	require.NoError(t, getChannelAffinityCache().SetWithTTL(suffix, 601, 10*time.Second))
	require.NoError(t, occupancyAddKeyFP(601, "fp1a2b3c4", 10*time.Second))
	require.NoError(t, lastBindSet(suffix, 601, 20*time.Second))

	RollbackChannelAffinityOnFinalFailure(ctx)

	_, found, err := getChannelAffinityCache().Get(suffix)
	require.NoError(t, err)
	assert.False(t, found, "final failure must roll back forward binding even after retry switch")

	count, _, err := occupancyBindingCount(601)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	_, found, err = lastBindGet(suffix)
	require.NoError(t, err)
	assert.False(t, found)
}

// TestRollbackOnFinalFailure_NoMeta gin context 无 affinity meta 时直接返回，无副作用。
func TestRollbackOnFinalFailure_NoMeta(t *testing.T) {
	setupRecordAffinityTest(t)

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	assert.NotPanics(t, func() {
		RollbackChannelAffinityOnFinalFailure(ctx)
	})
}

// TestRollbackOnFinalFailure_SwitchOnSuccessDisabled SwitchOnSuccess 关闭时，
// 终态失败同样回滚占位（回滚判定与 SwitchOnSuccess 无关，仅看终态失败出口）。
func TestRollbackOnFinalFailure_SwitchOnSuccessDisabled(t *testing.T) {
	suffix := fmt.Sprintf("final-switch-off:default:fp-%d", time.Now().UnixNano())
	meta := recordAffinityMeta("fp1a2b3c4", suffix)
	ctx := buildRecordAffinityContext(t, meta, 601)
	ctx.Set("channel_id", 602)
	setupRecordAffinityTest(t)

	setting := operation_setting.GetChannelAffinitySetting()
	setting.SwitchOnSuccess = false
	cleanupForwardAndLastBind(t, suffix)

	require.NoError(t, getChannelAffinityCache().SetWithTTL(suffix, 601, 10*time.Second))
	require.NoError(t, occupancyAddKeyFP(601, "fp1a2b3c4", 10*time.Second))
	require.NoError(t, lastBindSet(suffix, 601, 20*time.Second))

	RollbackChannelAffinityOnFinalFailure(ctx)

	_, found, err := getChannelAffinityCache().Get(suffix)
	require.NoError(t, err)
	assert.False(t, found, "final failure rolls back placement regardless of SwitchOnSuccess")
}

// TestRecordChannelAffinityDisabled 不启用亲和时 RecordChannelAffinity 不登记索引。
func TestRecordChannelAffinityDisabled(t *testing.T) {
	suffix := fmt.Sprintf("disabled-rule:default:fp-%d", time.Now().UnixNano())
	meta := recordAffinityMeta("fp1a2b3c4", suffix)
	ctx := buildRecordAffinityContext(t, meta, 0)
	setupRecordAffinityTest(t)
	cleanupForwardAndLastBind(t, suffix)

	setting := operation_setting.GetChannelAffinitySetting()
	setting.Enabled = false

	RecordChannelAffinity(ctx, 501)

	count, _, err := occupancyBindingCount(501)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	_, found, err := lastBindGet(suffix)
	require.NoError(t, err)
	assert.False(t, found)
}
