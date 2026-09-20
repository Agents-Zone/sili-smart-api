package service

// 真实 Redis 集成测试：验证 v2 占用索引（成员级过期 + Lua 原子操作）在真实
// redis-server 下的行为。miniredis 对 Lua/EVALSHA/时序的仿真与真实服务器存在
// 差异（脚本缓存、复制语义、时钟），生产路径必须以真实服务器回归。
// 通过环境变量 AFFINITY_REAL_REDIS（如 redis://127.0.0.1:16379/9）启用，
// 未设置时跳过。使用独立 DB 编号避免污染其它数据。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useRealRedisMode 连接 AFFINITY_REAL_REDIS 指向的真实 Redis 并切换全局存储，
// 清空该 DB 的亲和命名空间键后返回客户端。测试结束恢复原全局并清空本测试键。
func useRealRedisMode(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("AFFINITY_REAL_REDIS")
	if addr == "" {
		t.Skip("AFFINITY_REAL_REDIS not set; skipping real-redis integration test")
	}
	opt, err := redis.ParseURL(addr)
	require.NoError(t, err)
	client := redis.NewClient(opt)
	_, err = client.Ping(context.Background()).Result()
	require.NoError(t, err, "real redis must be reachable at %s", addr)

	prevEnabled := common.RedisEnabled
	prevRDB := common.RDB
	common.RedisEnabled = true
	common.RDB = client
	resetExclusiveStoreSingletons()
	resetAffinityCacheSingleton()

	// 清空亲和三命名空间（按前缀扫描删除，不动其它键）。
	ctx := context.Background()
	for _, ns := range []string{
		channelAffinityCacheNamespace,
		channelAffinityOccupancyNamespace,
		channelAffinityLastBindNamespace,
	} {
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, ns+":*", 1000).Result()
			require.NoError(t, err)
			if len(keys) > 0 {
				require.NoError(t, client.Unlink(ctx, keys...).Err())
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	}

	t.Cleanup(func() {
		_, _ = client.Unlink(ctx,
			channelAffinityOccupancyNamespace+":*").Result()
		common.RedisEnabled = prevEnabled
		common.RDB = prevRDB
		resetExclusiveStoreSingletons()
		resetAffinityCacheSingleton()
		_ = client.Close()
	})
	return client
}

// realRedisRequestCtx 构造真实规则匹配请求。
func realRedisRequestCtx(key string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Affinity-Key", key)
	return ctx
}

// TestRealRedisOccupancyMemberExpiry 真实 Redis 下成员级过期：
// 短 TTL 成员过期后占用计数归零、条目随之删除，不阻碍后续独占 claim。
func TestRealRedisOccupancyMemberExpiry(t *testing.T) {
	useRealRedisMode(t)

	require.NoError(t, occupancyAddKeyFP(881, "real-short", 500*time.Millisecond))
	require.NoError(t, occupancyAddKeyFP(881, "real-keep", 5*time.Second))

	count, _, err := occupancyBindingCount(881)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	time.Sleep(700 * time.Millisecond)

	count, fps, err := occupancyBindingCount(881)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "expired member must be pruned on real redis")
	assert.ElementsMatch(t, []string{"real-keep"}, fps)

	// 过期成员不得阻碍新键 claim 该渠道（条目仍有 real-keep，claim 应冲突）。
	claim, err := occupancyClaimExclusive(881, "real-new", 5*time.Second)
	require.NoError(t, err)
	assert.False(t, claim.Claimed, "live member must still block claim")
	assert.Equal(t, 1, claim.HolderCount)

	require.NoError(t, occupancyRemoveKeyFP(881, "real-keep"))
	count, _, err = occupancyBindingCount(881)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "last member removal must drop the entry")
}

// TestRealRedisV1EntryUpgrade 真实 Redis 上 v1 数组格式条目（升级前写入）被
// v2 读写路径正确承接：读取视为存活、claim 冲突、登记后重写为 v2 map 格式。
func TestRealRedisV1EntryUpgrade(t *testing.T) {
	client := useRealRedisMode(t)
	cache := getChannelAffinityOccupancyCache()
	ctx := context.Background()

	// 直接写入 v1 格式（绕过 codec，模拟升级前残留）。
	v1Key := cache.FullKey("882")
	require.NoError(t, client.Set(ctx, v1Key, `{"key_fps":["v1-a","v1-b"]}`, 30*time.Second).Err())

	count, fps, err := occupancyBindingCount(882)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "v1 members must be readable as live")
	assert.ElementsMatch(t, []string{"v1-a", "v1-b"}, fps)

	// v1 成员仍按存活参与独占判定。
	claim, err := occupancyClaimExclusive(882, "v2-new", 30*time.Second)
	require.NoError(t, err)
	assert.False(t, claim.Claimed, "v1 members must block claim until rewritten")

	// 登记触发重写为 v2 格式（v1 成员全部保留转入 map 形态）。
	require.NoError(t, occupancyAddKeyFP(882, "v1-a", 30*time.Second))
	raw, err := client.Get(ctx, v1Key).Result()
	require.NoError(t, err)
	t.Logf("entry after rewrite: %s", raw)
	var probe struct {
		KeyFPs map[string]int64 `json:"key_fps"`
	}
	require.NoError(t, common.Unmarshal([]byte(raw), &probe))
	assert.Equal(t, 2, len(probe.KeyFPs), "rewrite must keep all v1 members in v2 map form")
	assert.Contains(t, probe.KeyFPs, "v1-a")
	assert.Contains(t, probe.KeyFPs, "v1-b")
}

// TestRealRedisExclusiveBatchBalanced 真实 Redis 下 8 键 6 渠道独占批量：
// 分布不得堆积（单渠道 ≤2），总量守恒。复用软落位夹具的渠道与规则（内存库
// 候选集 + 真实 Redis 存储）。
func TestRealRedisExclusiveBatchBalanced(t *testing.T) {
	useRealRedisMode(t)
	useSoftPlacementFixture(t, true)

	keys := []string{"rr-1", "rr-2", "rr-3", "rr-4", "rr-5", "rr-6", "rr-7", "rr-8"}
	for _, key := range keys {
		mirrorDistributorRequest(t, key)
	}

	counts := map[int]int{}
	total := 0
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		counts[id] = c
		total += c
	}
	t.Logf("real-redis distribution: %v", counts)

	assert.Equal(t, len(keys), total, "every key must be registered exactly once")
	maxC := 0
	for _, c := range counts {
		if c > maxC {
			maxC = c
		}
	}
	assert.LessOrEqual(t, maxC, 2, "8 keys over 6 free channels must not stack 3+")
}

// TestRealRedisConcurrentAcquireConvergence 真实 Redis 下同键并发 acquire 收敛：
// 全部调用返回同一渠道，占用恰登记一次（SetNX + claim 回滚的跨 goroutine 语义，
// 近似跨实例竞争的进程内形态）。
func TestRealRedisConcurrentAcquireConvergence(t *testing.T) {
	useRealRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()

	meta := exclusiveAcquireTestMeta()
	meta.CacheKey = channelAffinityCacheNamespace + ":real-redis-rule:default:conc-key"
	meta.KeyFingerprint = "realconc0001"
	suffix := exclusiveCacheKeySuffix(meta)
	t.Cleanup(func() {
		_, _ = getChannelAffinityCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"701", "702"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{suffix})
	})

	const n = 8
	results := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			setChannelAffinityContext(ctx, meta)
			ch, found := acquireExclusiveBinding(ctx, meta, meta.UsingGroup, meta.ModelName)
			require.True(t, found)
			results[idx] = ch
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 1; i < n; i++ {
		assert.Equal(t, results[0], results[i], "all concurrent acquires must converge to the winner channel")
	}
	count, fps, err := occupancyBindingCount(results[0])
	require.NoError(t, err)
	assert.Equal(t, 1, count, "winner fingerprint must be registered exactly once")
	assert.ElementsMatch(t, []string{meta.KeyFingerprint}, fps)

	// 幽灵占用守恒：败选渠道不得残留任何指纹。
	for _, id := range []int{701, 702} {
		if id == results[0] {
			continue
		}
		c, _, err := occupancyBindingCount(id)
		require.NoError(t, err)
		assert.Equal(t, 0, c, "loser channel must have no ghost occupancy")
	}
}

// 防止未启用时 miniredis 引用被编译器裁剪的占位（保持 import 集合稳定）。
var _ = miniredis.RunT
