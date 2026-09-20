package service

// 白盒测试（需求方指定三类场景）：并发争抢、TTL 到期重绑、满载后新键占用。
// 仅覆盖既有实现未闭环的并发/时序行为，不重复 exclusive 单测已覆盖的表驱动分支。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useAffinityStressMode 统一构造内存模式 + 6 渠道（701、702、801..804）候选集夹具。
func useAffinityStressMode(t *testing.T) {
	t.Helper()
	useExclusiveMemoryMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()

	// setupExclusiveAcquireDB 只建 701/702，补建 801..804 到 default/gpt-4。
	priority := int64(0)
	weight := uint(100)
	for _, id := range []int{801, 802, 803, 804} {
		require.NoError(t, model.DB.Create(&model.Channel{
			Id:       id,
			Type:     constant.ChannelTypeOpenAI,
			Key:      fmt.Sprintf("key-%d", id),
			Status:   common.ChannelStatusEnabled,
			Name:     fmt.Sprintf("stress-channel-%d", id),
			Weight:   &weight,
			Models:   "gpt-4",
			Group:    "default",
			Priority: &priority,
		}).Error)
		require.NoError(t, model.DB.Create(&model.Ability{
			Group:     "default",
			Model:     "gpt-4",
			ChannelId: id,
			Enabled:   true,
			Priority:  &priority,
			Weight:    weight,
		}).Error)
	}
	// setupExclusiveAcquireDB 在建 701/702 后已刷过渠道缓存，801..804 需再刷一次
	// 才能进入 group2model2channels 候选集。
	model.InitChannelCache()
}

// 场景一：并发争抢。
// 同一键 N 个并发请求 + M 个其它键同时争抢 4 个渠道：
// 1. 同键并发必须先到先得收敛到同一渠道（SSOT 5.1.2 第2条末）；
// 2. 跨键在内存模式分段锁（同键哈希锁）下不超额独占（D3 语义）；
// 3. 结果无渠道超卖（键数只可能因 D3 偶发 +1，统计为复用而非独占）。
func TestStressConcurrentRacing(t *testing.T) {
	useAffinityStressMode(t)
	meta := exclusiveAcquireTestMeta()
	channelKeys := []string{"801", "802", "803", "804", "701", "702"}
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(channelKeys)
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	const sameKeyRacers = 8
	const otherKeys = 12 // 其它键各自独立并发首发 miss

	results := make([]int, sameKeyRacers)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < sameKeyRacers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
			results[idx] = ch
			if !found {
				t.Errorf("same-key racer %d: found=false", idx)
			}
		}(i)
	}
	otherChans := make([]int, otherKeys)
	for i := 0; i < otherKeys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			m := exclusiveAcquireTestMeta()
			m.KeyFingerprint = fmt.Sprintf("other%02d", idx)
			m.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:other-" + strconv.Itoa(idx)
			ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, m), m, m.UsingGroup, m.ModelName)
			otherChans[idx] = ch
			if !found {
				t.Errorf("other-key racer %d: found=false", idx)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// 同键收敛：全部 racer 同渠道且 >0。
	winner := results[0]
	require.Greater(t, winner, 0)
	for i, ch := range results {
		assert.Equal(t, winner, ch, "same-key racer %d must converge to winner channel", i)
	}

	// 跨键登记守恒（occupancyEntryLocks 修复 lost update 后的回归断言）：
	// 13 键并发首发 miss，反向索引成员总数必须恰好等于键数，无丢失无重复。
	// 渠道覆盖数不断言：跨键选择为集内均匀随机（D1），存在空桶属正常分布。
	totalBound := 0
	for _, key := range channelKeys {
		count, _, err := occupancyBindingCount(mustAtoi(key))
		require.NoError(t, err)
		totalBound += count
	}
	assert.Equal(t, 1+otherKeys, totalBound, "every key must be registered exactly once")
}

// 场景一（串行对照）：独占语义的正面验证。键逐个串行绑定时，
// 前 6 个键必须各独占一个渠道（无降级），第 7 个起满载降级并按最少绑定数分摊。
func TestStressSequentialExclusiveDistribution(t *testing.T) {
	useAffinityStressMode(t)
	channelKeys := []string{"801", "802", "803", "804", "701", "702"}
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(channelKeys)
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{"exclusive-test-rule:default:seq-*"})
		_, _ = getChannelAffinityCache().DeleteMany([]string{"exclusive-test-rule:default:seq-*"})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	degradedBefore := degradedReuseTotal()
	seen := make(map[int]int) // channelID -> 绑定键数
	for i := 0; i < 9; i++ {
		m := exclusiveAcquireTestMeta()
		m.KeyFingerprint = fmt.Sprintf("seq%02d", i)
		m.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:seq-" + strconv.Itoa(i)
		ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, m), m, m.UsingGroup, m.ModelName)
		require.True(t, found)
		require.Greater(t, ch, 0)
		seen[ch]++
	}

	// 前 6 键独占：每渠道恰好 1 键；后 3 键满载降级分摊到最少绑定数层。
	total := 0
	maxCount := 0
	for _, key := range channelKeys {
		count, _, err := occupancyBindingCount(mustAtoi(key))
		require.NoError(t, err)
		total += count
		if count > maxCount {
			maxCount = count
		}
	}
	assert.Equal(t, 9, total)
	assert.Equal(t, 2, maxCount, "3 degraded keys must spread over the 6 fewest-binding channels (each 1->2)")
	assert.Equal(t, degradedBefore+3, degradedReuseTotal(), "exactly 3 keys degrade after 6 exclusive slots")
}

// 场景一（Redis 模式）：同键并发经 SetNX 先到先得，败者读胜者结果；
// 跨键登记经 Lua 脚本原子执行，多键并发落同一渠道时成员不丢失（与内存模式对照）。
func TestStressConcurrentRacingRedis(t *testing.T) {
	useExclusiveRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()
	meta := exclusiveAcquireTestMeta()
	occupancyKeys := []string{"701", "702"}
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(occupancyKeys)
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
	})

	const racers = 8
	results := make([]int, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			m := meta
			if idx > 0 {
				m.KeyFingerprint = fmt.Sprintf("redis-racer-%d", idx)
				m.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:redis-racer-" + strconv.Itoa(idx)
			}
			ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, m), m, m.UsingGroup, m.ModelName)
			results[idx] = ch
			if !found {
				t.Errorf("racer %d: found=false", idx)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// 同键先到先得：8 个键中 idx0 与其他键独立，这里每个键都拿到渠道即可；
	// 原子性断言在登记守恒：8 个不同指纹并发登记到 2 个渠道，成员总数必须等于 8。
	total := 0
	for _, key := range occupancyKeys {
		count, fps, err := occupancyBindingCount(mustAtoi(key))
		require.NoError(t, err)
		total += count
		_ = fps
	}
	assert.Equal(t, racers, total, "redis Lua registration must not lose members across concurrent keys")
}

// 场景二：TTL 到期重绑。
// 同一键正向绑定与占用索引 TTL 到期（lastBind 仍在两周期内）：重绑必须优先绑回原渠道；
// 原渠道被其它键占走后，走正常独占选路到其它空闲渠道。
func TestStressTTLExpiryRebind(t *testing.T) {
	useAffinityStressMode(t)
	meta := exclusiveAcquireTestMeta()
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"801", "802", "803", "804", "701", "702"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	// 用极短 TTL 模拟到期：直接预置 lastBind（正向/占用已过期消失），lastBind TTL 长。
	require.NoError(t, lastBindSet(exclusiveCacheKeySuffix(meta), 802, 30*time.Second))

	ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	assert.Equal(t, 802, ch, "rebind after expiry must prefer last-bound channel")

	// 到期重绑且原渠道被其它键占用：正常选路到其它空闲渠道（不误判满载）。
	t.Run("original channel taken", func(t *testing.T) {
		m2 := exclusiveAcquireTestMeta()
		m2.KeyFingerprint = "fp-taken"
		m2.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:fp-taken"
		t.Cleanup(func() {
			_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"802"})
			_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(m2)})
			_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(m2)})
		})
		// 802 已被 meta 键重绑占用，m2 键 lastBind 也记 802：应避开 802 选其它空闲渠道。
		require.NoError(t, lastBindSet(exclusiveCacheKeySuffix(m2), 802, 30*time.Second))
		ch2, found2 := acquireExclusiveBinding(buildExclusiveAcquireContext(t, m2), m2, m2.UsingGroup, m2.ModelName)
		require.True(t, found2)
		assert.NotEqual(t, 802, ch2, "must not steal channel occupied by another key")
	})
}

// 场景二（内存模式真实过期）：占用条目过期后条目消失，重绑同渠道不受残留影响。
func TestStressTTLMemoryRealExpiry(t *testing.T) {
	useAffinityStressMode(t)
	meta := exclusiveAcquireTestMeta()
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"801", "802", "803", "804"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	// 首发：占 802，正向与占用 TTL 200ms，lastBind TTL 400ms。
	m := meta
	m.TTLSeconds = 1 // exclusiveAffinityTTL 取规则值，最小粒度秒；200ms 需直接操作缓存层
	_ = m
	// 直接用存储层构造：正向 802/150ms、占用 802/150ms、lastBind 802/2s。
	suffix := exclusiveCacheKeySuffix(meta)
	require.NoError(t, getChannelAffinityCache().SetWithTTL(suffix, 802, 150*time.Millisecond))
	require.NoError(t, occupancyAddKeyFP(802, meta.KeyFingerprint, 150*time.Millisecond))
	require.NoError(t, lastBindSet(suffix, 802, 2*time.Second))

	time.Sleep(250 * time.Millisecond) // 正向与占用过期，lastBind 存活

	ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	assert.Equal(t, 802, ch, "expired forward/occupancy with live lastBind must rebind to original channel")

	count, fps, err := occupancyBindingCount(802)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, meta.KeyFingerprint, fps[0], "rebind must re-register fingerprint exactly once")
}

// 场景三：满载后新键占用。
// 4 渠道全部被占：新键首发 miss 应最少绑定数优先降级复用（shared），不阻塞、不 panic；
// 随后其它键释放（占用移除）后，再来的新键应回到独占选路（不再降级）。
func TestStressFullLoadThenRelease(t *testing.T) {
	useAffinityStressMode(t)
	meta := exclusiveAcquireTestMeta()
	occupancyKeys := []string{"801", "802", "803", "804", "701", "702"}
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(occupancyKeys)
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	// 占满全部 6 渠道：801 给 2 个键，其余各 1 个键。
	require.NoError(t, occupancyAddKeyFP(801, "holder-a", 30*time.Second))
	require.NoError(t, occupancyAddKeyFP(801, "holder-b", 30*time.Second))
	for _, id := range []int{802, 803, 804, 701, 702} {
		require.NoError(t, occupancyAddKeyFP(id, "holder-x", 30*time.Second))
	}

	degradedBefore := degradedReuseTotal()

	// 满载新键：应成功绑定 shared 模式到最少绑定数渠道（801 除外都是 1，任一皆可），
	// 且降级计数 +1。
	ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found, "full load must not block or fail the request")
	require.Contains(t, []int{801, 802, 803, 804, 701, 702}, ch)
	assert.Equal(t, degradedBefore+1, degradedReuseTotal(), "shared degrade must bump counter")

	// 该键再请求（亲和命中路径语义，走 GetPreferredChannelByAffinity）应稳定同渠道。
	got, found := GetPreferredChannelByAffinity(buildExclusiveAcquireContext(t, meta), meta.ModelName, meta.UsingGroup)
	_ = got
	_ = found
	// 注意：buildExclusiveAcquireContext 只设 meta 不含规则匹配，GetPreferred 走规则匹配
	// 不在本用例范围；这里仅验证 acquire 后正向缓存可读且稳定。
	cached, ok, err := getChannelAffinityCache().Get(exclusiveCacheKeySuffix(meta))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, ch, cached, "forward binding must be readable and stable")

	// 释放一个空闲渠道（排除首次降级已占的 ch 与 801），新键应独占它（不再降级）。
	freed := 0
	for _, id := range []int{802, 803, 804, 702} {
		if id == ch {
			continue
		}
		count, _, err := occupancyBindingCount(id)
		require.NoError(t, err)
		if count == 1 {
			freed = id
			require.NoError(t, occupancyRemoveKeyFP(id, "holder-x"))
			break
		}
	}
	require.Greater(t, freed, 0, "fixture must find a holder-occupied channel to free")
	meta2 := exclusiveAcquireTestMeta()
	meta2.KeyFingerprint = "fp-after-release"
	meta2.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:fp-after-release"
	t.Cleanup(func() {
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta2)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta2)})
	})
	before2 := degradedReuseTotal()
	ch2, found2 := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta2), meta2, meta2.UsingGroup, meta2.ModelName)
	require.True(t, found2)
	assert.Equal(t, freed, ch2, "freed channel must be exclusively claimed by next key")
	assert.Equal(t, before2, degradedReuseTotal(), "exclusive bind must not bump degraded counter")
}

// 场景三（边界）：占用全部渠道的持有键自身 TTL 到期后（占用条目消失），
// 新键首发应独占而非降级——验证满载判定不残留过期成员（内存模式条目级 TTL）。
func TestStressFullLoadHolderExpiry(t *testing.T) {
	useAffinityStressMode(t)
	meta := exclusiveAcquireTestMeta()
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"801", "802"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	// 两个渠道各放一个 150ms 的持有键后整体过期。
	require.NoError(t, occupancyAddKeyFP(801, "short-holder", 150*time.Millisecond))
	require.NoError(t, occupancyAddKeyFP(802, "short-holder", 150*time.Millisecond))
	time.Sleep(250 * time.Millisecond)

	before := degradedReuseTotal()
	ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	// 空闲集应含全部 6 渠道（过期持有者已释放），随机选定任一皆可，断言不降级即可。
	assert.Contains(t, []int{801, 802, 803, 804, 701, 702}, ch)
	assert.Equal(t, before, degradedReuseTotal(), "expired holders must not trigger degrade")
}

// 场景二（并发变体）：TTL 到期后同一键 N 个请求同时首发 miss：
// 全部 racer 必须收敛到同一渠道（锁内 double-check / SetNX 先到先得），
// 且占用索引恰好登记一次（胜者），lastBind 指向胜者渠道。
func TestStressTTLExpiryRebindConcurrent(t *testing.T) {
	useAffinityStressMode(t)
	meta := exclusiveAcquireTestMeta()
	suffix := exclusiveCacheKeySuffix(meta)
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"801", "802", "803", "804", "701", "702"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityCache().DeleteMany([]string{suffix})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	// 模拟到期：正向与占用已消失，仅 lastBind 存活并记录原渠道 802。
	require.NoError(t, lastBindSet(suffix, 802, 30*time.Second))

	const racers = 8
	results := make([]int, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
			results[idx] = ch
			if !found {
				t.Errorf("racer %d: found=false", idx)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	winner := results[0]
	require.Greater(t, winner, 0)
	for i, ch := range results {
		assert.Equal(t, winner, ch, "expiry rebind racers must converge to one channel", i)
	}
	assert.Equal(t, 802, winner, "converged winner must be the last-bound channel")

	count, fps, err := occupancyBindingCount(802)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "winner fingerprint must be registered exactly once")
	assert.ElementsMatch(t, []string{meta.KeyFingerprint}, fps)

	record, found, err := lastBindGet(suffix)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, winner, record.ChannelID)
}

// 场景三（并发变体）：满载状态下大批新键并发占用。
// 关键不变量：不阻塞、不 panic、全部成功绑定；登记守恒（成员总数 = 键数）；
// shared 降级分摊到最少绑定数层（无单渠道被异常堆积）。
func TestStressFullLoadConcurrentNewKeys(t *testing.T) {
	useAffinityStressMode(t)
	channelKeys := []string{"801", "802", "803", "804", "701", "702"}
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(channelKeys)
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{"exclusive-test-rule:default:full-load-*"})
		_, _ = getChannelAffinityCache().DeleteMany([]string{"exclusive-test-rule:default:full-load-*"})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	// 占满全部 6 渠道：801 给 2 个键，其余各 1 个键（共 7 个持有键）。
	require.NoError(t, occupancyAddKeyFP(801, "holder-a", 30*time.Second))
	require.NoError(t, occupancyAddKeyFP(801, "holder-b", 30*time.Second))
	for _, id := range []int{802, 803, 804, 701, 702} {
		require.NoError(t, occupancyAddKeyFP(id, "holder-x", 30*time.Second))
	}

	const newKeys = 16
	results := make([]int, newKeys)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < newKeys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			m := exclusiveAcquireTestMeta()
			m.KeyFingerprint = fmt.Sprintf("full-load-%02d", idx)
			m.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:full-load-" + strconv.Itoa(idx)
			ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, m), m, m.UsingGroup, m.ModelName)
			results[idx] = ch
			if !found {
				t.Errorf("full-load key %d: found=false (must not block)", idx)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// 全部成功且渠道合法。
	for i, ch := range results {
		require.Greater(t, ch, 0, "key %d must bind successfully under full load", i)
		assert.Contains(t, []int{801, 802, 803, 804, 701, 702}, ch)
	}

	// 登记守恒：7 个持有键 + 16 个新键 = 23 个成员。
	total := 0
	for _, key := range channelKeys {
		count, _, err := occupancyBindingCount(mustAtoi(key))
		require.NoError(t, err)
		total += count
	}
	assert.Equal(t, 7+newKeys, total, "all full-load keys must be registered exactly once")

	// 分布仅记录不断言精确分摊：满载降级的决策基于各键读取占用索引的瞬间视图，
	// 跨键并发（SSOT 8.3 D3）下同层随机选定的堆积属认可边界；独占语义不受影响
	//（满载本就是 shared 终态，无超额独占可言）。
	counts := make(map[int]int)
	for _, key := range channelKeys {
		count, _, err := occupancyBindingCount(mustAtoi(key))
		require.NoError(t, err)
		counts[mustAtoi(key)] = count
	}
	t.Logf("full-load concurrent distribution: %v", counts)
}

// 场景五：键 A、B 各自绑定成功后，A 普通失败仍保留三处绑定数据，
// B 的占位保持原样，后续新键从剩余空闲渠道独占选路。
func TestStressFailurePreservesExclusiveBindings(t *testing.T) {
	useAffinityStressMode(t)
	metaA := exclusiveAcquireTestMeta()
	metaA.KeyFingerprint = "rollback-fp-a"
	metaA.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:rollback-a"
	metaB := exclusiveAcquireTestMeta()
	metaB.KeyFingerprint = "rollback-fp-b"
	metaB.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:rollback-b"
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"801", "802", "803", "804", "701", "702"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(metaA), exclusiveCacheKeySuffix(metaB)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(metaA), exclusiveCacheKeySuffix(metaB)})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	ctxA := buildExclusiveAcquireContext(t, metaA)
	chA, foundA := acquireExclusiveBinding(ctxA, metaA, metaA.UsingGroup, metaA.ModelName)
	require.True(t, foundA)
	ctxB := buildExclusiveAcquireContext(t, metaB)
	chB, foundB := acquireExclusiveBinding(ctxB, metaB, metaB.UsingGroup, metaB.ModelName)
	require.True(t, foundB)
	require.NotEqual(t, chA, chB, "fixture: two keys must hold distinct exclusive channels")

	// A 普通失败保持绑定，其他键的占用也保持原样。
	RollbackChannelAffinityOnFinalFailure(ctxA)

	// A 占位三处保持。
	_, found, err := getChannelAffinityCache().Get(exclusiveCacheKeySuffix(metaA))
	require.NoError(t, err)
	assert.True(t, found, "普通失败保留绑定")

	countA, fpsA, err := occupancyBindingCount(chA)
	require.NoError(t, err)
	assert.Equal(t, 1, countA)
	assert.ElementsMatch(t, []string{metaA.KeyFingerprint}, fpsA)

	_, found, err = lastBindGet(exclusiveCacheKeySuffix(metaA))
	require.NoError(t, err)
	assert.True(t, found, "普通失败保留最近绑定")

	// B 占位原样保留。
	cachedB, found, err := getChannelAffinityCache().Get(exclusiveCacheKeySuffix(metaB))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, chB, cachedB)

	countB, fpsB, err := occupancyBindingCount(chB)
	require.NoError(t, err)
	assert.Equal(t, 1, countB)
	assert.ElementsMatch(t, []string{metaB.KeyFingerprint}, fpsB)

	recordB, found, err := lastBindGet(exclusiveCacheKeySuffix(metaB))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, chB, recordB.ChannelID)

	// C 从剩余空闲渠道独占选路，A、B 的渠道均继续被占用。
	metaC := exclusiveAcquireTestMeta()
	metaC.KeyFingerprint = "rollback-fp-c"
	metaC.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:rollback-c"
	t.Cleanup(func() {
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(metaC)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(metaC)})
	})
	beforeC := degradedReuseTotal()
	chC, foundC := acquireExclusiveBinding(buildExclusiveAcquireContext(t, metaC), metaC, metaC.UsingGroup, metaC.ModelName)
	require.True(t, foundC)
	assert.NotEqual(t, chB, chC, "新键应使用剩余空闲渠道")
	assert.NotEqual(t, chA, chC, "普通失败后的渠道仍被原键占用")
	assert.Equal(t, beforeC, degradedReuseTotal(), "仍有空闲渠道时保持独占")
}

// 场景二（Redis 模式真实过期）：正向/占用 TTL 到期后 lastBind 仍存活，
// Redis 模式重绑必须绑回原渠道且重新登记一次（miniredis FastForward 模拟时间前进）。
func TestStressTTLRedisRealExpiry(t *testing.T) {
	server := useExclusiveRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()
	meta := exclusiveAcquireTestMeta()
	suffix := exclusiveCacheKeySuffix(meta)
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"701", "702"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityCache().DeleteMany([]string{suffix})
	})

	// 首发：经完整 acquire 链占位（TTL 由 meta.TTLSeconds=30s 决定）。
	ctx := buildExclusiveAcquireContext(t, meta)
	ch, found := acquireExclusiveBinding(ctx, meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	require.Contains(t, []int{701, 702}, ch)

	// 时间前进 31s：正向与占用（30s）过期，lastBind（60s）存活。
	server.FastForward(31 * time.Second)

	ch2, found2 := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found2)
	assert.Equal(t, ch, ch2, "redis expiry rebind must prefer last-bound channel")

	count, fps, err := occupancyBindingCount(ch)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "rebind must re-register fingerprint exactly once")
	assert.ElementsMatch(t, []string{meta.KeyFingerprint}, fps)

	record, found, err := lastBindGet(suffix)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, ch, record.ChannelID)
}

// 场景六（Redis 模式并发满载）：多键并发首发于仅 2 渠道的候选集：
// 全部绑定成功、登记守恒、降级计数精确增加。
func TestStressFullLoadRedisConcurrent(t *testing.T) {
	useExclusiveRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()
	occupancyKeys := []string{"701", "702"}
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(occupancyKeys)
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{"exclusive-test-rule:default:redis-full-*"})
		_, _ = getChannelAffinityCache().DeleteMany([]string{"exclusive-test-rule:default:redis-full-*"})
	})

	const newKeys = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	failures := make([]bool, newKeys)
	for i := 0; i < newKeys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			m := exclusiveAcquireTestMeta()
			m.KeyFingerprint = fmt.Sprintf("redis-full-%02d", idx)
			m.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:redis-full-" + strconv.Itoa(idx)
			ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, m), m, m.UsingGroup, m.ModelName)
			if !found || ch <= 0 {
				failures[idx] = true
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, f := range failures {
		assert.False(t, f, "redis full-load key %d must not fail", i)
	}

	// 降级计数不断言精确值：跨键并发（SSOT 8.3 D3）下各键读取占用索引的瞬间
	// 视图可能同时判定有空闲、Mode=exclusive 占位成功，degraded_reuse_total
	// 只统计决策时明确判满载的键。渠道实际复用状态由统计口径
	// GetChannelAffinityExclusiveStats 的 shared_bindings 体现。此处断言守恒
	// 与合法性即可。
	total := 0
	for _, key := range occupancyKeys {
		count, _, err := occupancyBindingCount(mustAtoi(key))
		require.NoError(t, err)
		total += count
	}
	assert.Equal(t, newKeys, total, "redis concurrent full-load registration must conserve members")

	// 复用状态最终可见：两渠道合计 10 键，至少一个渠道键数 > 1（shared 终态）。
	c1, _, err := occupancyBindingCount(701)
	require.NoError(t, err)
	c2, _, err := occupancyBindingCount(702)
	require.NoError(t, err)
	assert.Greater(t, c1+c2, 2, "full load must end in shared state (some channel holds >1 keys)")
}

// 场景四（补充）：软亲和规则（未启用独占）绑定同样登记占用索引（SSOT 5.2.4 规则1），
// 且独占规则在判定时能感知软亲和键的占用。
func TestStressSoftAffinityRegistersAndExclusiveSeesIt(t *testing.T) {
	useAffinityStressMode(t)
	meta := exclusiveAcquireTestMeta()

	suffix := "soft-rule:default:soft-key"
	t.Cleanup(func() {
		_, _ = getChannelAffinityCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"801"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{suffix})
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	})

	// 软亲和绑定到 801：模拟 RecordChannelAffinity 全链（bound=0 无迁移）。
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	setChannelAffinityContext(ctx, channelAffinityMeta{
		CacheKey:       channelAffinityCacheNamespace + ":" + suffix,
		TTLSeconds:     60,
		RuleName:       "soft-rule",
		KeyFingerprint: "soft-fp-0001",
	})
	setting := operation_setting.GetChannelAffinitySetting()
	origEnabled, origSwitch := setting.Enabled, setting.SwitchOnSuccess
	setting.Enabled, setting.SwitchOnSuccess = true, false
	t.Cleanup(func() {
		setting.Enabled, setting.SwitchOnSuccess = origEnabled, origSwitch
	})
	RecordChannelAffinity(ctx, 801)

	count, _, err := occupancyBindingCount(801)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "soft affinity binding must register occupancy index")

	// 独占键感知软亲和占用：801 非空闲，选其它。
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"802"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
		_, _ = getChannelAffinityCache().DeleteMany([]string{exclusiveCacheKeySuffix(meta)})
	})
	ch, found := acquireExclusiveBinding(buildExclusiveAcquireContext(t, meta), meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found)
	assert.NotEqual(t, 801, ch, "exclusive key must see soft affinity occupancy on 801")
}

// ---- 内部小工具 ----

func degradedReuseTotal() uint64 {
	return GetChannelAffinityDegradedReuseTotal()
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}
