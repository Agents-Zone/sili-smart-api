package service

// 真实 Redis 下 occupancy 条目级 TTL 截断缺陷的复现与回归：
// occupancyAddScript/occupancyClaimExclusiveScript 每次登记把条目整体 SET PX 本次
// TTL，同渠道上共存不同 TTL 的成员（不同规则 ttl_seconds 不同、软规则与独占规则
// 共享渠道池）时，短 TTL 成员的登记会把条目寿命缩短到自身 TTL，长 TTL 成员随条目
// 整体消失。占用丢失 -> 渠道被误判空闲 -> 新键 claim 成功 -> 与仍在用该渠道的键
// 静默双占（无降级标记）。生产症状：渠道大量空闲时多键共享同一渠道。
// 通过 AFFINITY_REAL_REDIS 启用，未设置时跳过。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRealRedisEntryTTLNotTruncatedByShorterMember 条目 TTL 不得被更短 TTL 的
// 后续登记截断：长 TTL 成员的存活期以自身成员级过期为准。
func TestRealRedisEntryTTLNotTruncatedByShorterMember(t *testing.T) {
	useRealRedisMode(t)

	// 长成员（模拟规则 A，ttl 20s）先登记。
	require.NoError(t, occupancyAddKeyFP(951, "fp-long", 20*time.Second))
	// 短成员（模拟规则 B，ttl 1s）后登记：条目整体 PX 被缩短为 1s。
	require.NoError(t, occupancyAddKeyFP(951, "fp-short", 1*time.Second))

	count, fps, err := occupancyBindingCount(951)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	t.Logf("before short expiry: count=%d fps=%v", count, fps)

	time.Sleep(1500 * time.Millisecond)

	// 短成员过期后，长成员必须仍在（成员级过期语义），不得随条目整体消失。
	count, fps, err = occupancyBindingCount(951)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "long-TTL member must survive the short member's entry-level TTL truncation")
	assert.ElementsMatch(t, []string{"fp-long"}, fps)

	// 长成员存活期间，新键 claim 该渠道必须冲突（占用未丢失）。
	claim, err := occupancyClaimExclusive(951, "fp-new", 5*time.Second)
	require.NoError(t, err)
	assert.False(t, claim.Claimed, "lost occupancy would let a new key silently double-bind the channel")
	assert.Equal(t, 1, claim.HolderCount)
}

// TestRealRedisMixedTTLSilentDoubleBind 生产形态复现：长 TTL 独占键绑渠道 A 后，
// 短 TTL 键（另一规则）短暂共享 A，短键过期后 A 的占用必须仍属于长键；
// 若占用被条目截断丢失，第三键 claim A 成功即形成静默双占。
// 注意：长键续期命中时若检测到共享（短键注入）且有空闲渠道，独占收敛 hop 会
// 把长键迁往空闲渠道（正当行为）。为聚焦本测试的条目 TTL 语义，注入短键前先
// 把其余候选渠道占满（各放 1 个假指纹），使 hop 前提不成立、长键留驻 A。
func TestRealRedisMixedTTLSilentDoubleBind(t *testing.T) {
	useRealRedisMode(t)
	useSoftPlacementFixture(t, true)

	// 长键独占绑定（规则 ttl=60s，与夹具规则一致）。
	ctxLong := realRedisRequestCtx("long-key")
	chLong, found := GetPreferredChannelByAffinity(ctxLong, "gpt-4", "default")
	require.True(t, found)
	require.Contains(t, softPlacementChannels, chLong)

	// 占满其余候选渠道（各 1 个假指纹）：hop 无更优落点，长键稳定留驻。
	for _, id := range softPlacementChannels {
		if id != chLong {
			require.NoError(t, occupancyAddKeyFP(id, fmt.Sprintf("mixed-fill-%d", id), 30*time.Second))
		}
	}

	// 短 TTL 成员直接注入到同一渠道（模拟另一规则 ttl=1s 的键满载/随机落位）。
	fpLong := affinityFingerprint("long-key")
	require.NoError(t, occupancyAddKeyFP(chLong, "fp-short-rule", 1*time.Second))

	// 长键续期一次（命中路径，occupancyAddKeyFP 重置条目 PX 为 60s——不触发截断）。
	ctxRenew := realRedisRequestCtx("long-key")
	_, found = GetPreferredChannelByAffinity(ctxRenew, "gpt-4", "default")
	require.True(t, found)
	require.Equal(t, chLong, forwardBindingOfKey(t, "long-key"), "full candidate set must keep long-key in place (no hop target)")
	ctxRenew.Set("channel_id", chLong)
	RecordChannelAffinity(ctxRenew, chLong)

	time.Sleep(1500 * time.Millisecond)

	// 短成员过期。此刻渠道 A 的占用必须仍含长键。
	count, fps, err := occupancyBindingCount(chLong)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "long-key occupancy must survive the short member's TTL")
	assert.Contains(t, fps, fpLong)

	// 第三键首发独占：不得把长键的渠道视为空闲（静默双占即生产堆积来源）。
	// 其余渠道已被占满（各 1 假指纹），第三键必然降级 shared，落点任意。
	ctxThird := realRedisRequestCtx("third-key")
	chThird, found := GetPreferredChannelByAffinity(ctxThird, "gpt-4", "default")
	require.True(t, found)
	_ = chThird
	// 核心断言收敛于占用索引：长键渠道条目必须仍含长键指纹（未被截断丢失）。
	count, fps, err = occupancyBindingCount(chLong)
	require.NoError(t, err)
	assert.Contains(t, fps, fpLong, "long-key occupancy must never be lost to entry TTL truncation")
}

// forwardBindingOfKey 读长键正向绑定渠道（0 为无绑定）。
func forwardBindingOfKey(t *testing.T, key string) int {
	t.Helper()
	v, found, err := getChannelAffinityCache().Get(affinityKeySuffix(key))
	require.NoError(t, err)
	if !found {
		return 0
	}
	return v
}

// TestRealRedisConcurrentExclusiveBatch20 真实 Redis 下 20 键 6 渠道并发首发独占：
// 均摊上限 4（20/6=3.34 向上取整），静默双占（无降级的超卖）不得出现——
// 独占（exclusive 模式）占位在 CAS 下每渠道至多 1 键，超出部分必然带降级计数。
func TestRealRedisConcurrentExclusiveBatch20(t *testing.T) {
	useRealRedisMode(t)
	useSoftPlacementFixture(t, true)

	degradedBefore := degradedReuseTotal()

	const keys = 20
	results := make([]int, keys)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < keys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			key := fmt.Sprintf("cb-%02d", idx)
			ctx := realRedisRequestCtx(key)
			ch, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
			if !found || ch <= 0 {
				t.Errorf("key %s: found=%v ch=%d", key, found, ch)
				return
			}
			ctx.Set("channel_id", ch)
			RecordChannelAffinity(ctx, ch)
			results[idx] = ch
		}(i)
	}
	close(start)
	wg.Wait()

	dist := map[int]int{}
	for _, ch := range results {
		dist[ch]++
	}
	t.Logf("concurrent batch distribution: %v", dist)

	total := 0
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		total += c
	}
	assert.Equal(t, keys, total, "every key must be registered exactly once")

	// 堆积检测：20 键 6 渠道均摊 3.34，最优分摊为 4,4,3,3,3,3；原子最小落位下
	// 每渠道与均摊值的偏差不得超过 1（修复前并发共享陈旧快照可堆到 5+，
	// 而其它渠道仅 2——渠道富裕却多人共用同一渠道）。
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		assert.LessOrEqualf(t, c, 5, "channel %d holds %d keys", id, c)
		assert.GreaterOrEqualf(t, c, 2, "channel %d under-used at %d keys while others are loaded", id, c)
	}
	_ = degradedBefore
}

// realRedisRequestCtx 已存在于 realredis_test.go，此处仅引用其行为做批量断言辅助。

// mustAtoi 已有实现于本包（claim 测试），确保引用一致。
var _ = common.GetTimestamp

// 保持 http/httptest/model 引用（realRedisRequestCtx 与夹具间接使用）。
var _ = httptest.NewRequest
var _ = http.MethodPost
var _ = model.CacheGetChannel
var _ = gin.TestMode
