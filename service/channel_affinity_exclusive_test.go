package service

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
