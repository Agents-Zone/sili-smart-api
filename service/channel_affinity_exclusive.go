package service

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/cachex"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/samber/hot"
)

const (
	channelAffinityOccupancyNamespace = "new-api:channel_affinity_occupancy:v1"
	channelAffinityLastBindNamespace  = "new-api:channel_affinity_last_bind:v1"

	affinityBindModeExclusive = "exclusive"
	affinityBindModeShared    = "shared"

	exclusiveRedisOpTimeout = 2 * time.Second
)

// channelAffinityOccupancyEntry 为反向占用索引条目：渠道 -> 键指纹集合（SSOT 5.2.3）。
type channelAffinityOccupancyEntry struct {
	KeyFPs []string `json:"key_fps"`
}

// channelAffinityLastBindRecord 为最近绑定记录值结构（SSOT 5.1.3、04 文档 §4.2）。
type channelAffinityLastBindRecord struct {
	ChannelID int   `json:"channel_id"`
	BoundAt   int64 `json:"bound_at"`
}

var (
	channelAffinityOccupancyOnce  sync.Once
	channelAffinityOccupancyCache *cachex.HybridCache[channelAffinityOccupancyEntry]

	channelAffinityLastBindOnce  sync.Once
	channelAffinityLastBindCache *cachex.HybridCache[channelAffinityLastBindRecord]

	// occupancyMemExpireAt 记录内存模式各条目的过期时刻（key 为渠道 ID 十进制字符串），
	// 供 remove 写回时计算剩余 TTL（不续期）。条目自然过期时残留记录在下次登记/移除时覆盖或清除。
	occupancyMemExpireAt sync.Map
)

// occupancyAddScript 在 Redis 模式下原子完成"读条目 -> 登记成员 -> 写回/续期"（04 文档 §6.1）：
// KEYS[1] 为 occupancy 条目全键，ARGV[1] 为键指纹，ARGV[2] 为 TTL 毫秒。
// 集合已含该指纹时仅 PEXPIRE 续期整条目；否则追加成员并 SET PX 写回。
var occupancyAddScript = redis.NewScript(`
local raw = redis.call('GET', KEYS[1])
local fps = {}
if raw and raw ~= '' then
  local ok, obj = pcall(cjson.decode, raw)
  if ok and type(obj) == 'table' and type(obj.key_fps) == 'table' then
    fps = obj.key_fps
  end
end
local fp = ARGV[1]
for _, m in ipairs(fps) do
  if m == fp then
    redis.call('PEXPIRE', KEYS[1], ARGV[2])
    return 1
  end
end
fps[#fps + 1] = fp
redis.call('SET', KEYS[1], cjson.encode({key_fps = fps}), 'PX', ARGV[2])
return 1
`)

// newAffinityStoreCache 为独占运行时命名空间构造 HybridCache：容量与默认 TTL
// 取亲和设置，缺省回退 100_000 / 3600（与正向缓存 getter 的回退口径一致）。
func newAffinityStoreCache[V any](namespace string, codec cachex.ValueCodec[V]) *cachex.HybridCache[V] {
	setting := operation_setting.GetChannelAffinitySetting()
	capacity := setting.MaxEntries
	if capacity <= 0 {
		capacity = 100_000
	}
	defaultTTLSeconds := setting.DefaultTTLSeconds
	if defaultTTLSeconds <= 0 {
		defaultTTLSeconds = 3600
	}
	return cachex.NewHybridCache[V](cachex.HybridCacheConfig[V]{
		Namespace: cachex.Namespace(namespace),
		// 实时解析 RDB：缓存单例可能在 Redis 初始化前（启动加载钩子路径）
		// 构造，构造时捕获指针会把实例永久锁死在内存模式。
		RedisClient: func() *redis.Client {
			return common.RDB
		},
		RedisEnabled: func() bool {
			return common.RedisEnabled && common.RDB != nil
		},
		RedisCodec: codec,
		Memory: func() *hot.HotCache[string, V] {
			return hot.NewHotCache[string, V](hot.LRU, capacity).
				WithTTL(time.Duration(defaultTTLSeconds) * time.Second).
				WithJanitor().
				Build()
		},
	})
}

func getChannelAffinityOccupancyCache() *cachex.HybridCache[channelAffinityOccupancyEntry] {
	channelAffinityOccupancyOnce.Do(func() {
		channelAffinityOccupancyCache = newAffinityStoreCache(channelAffinityOccupancyNamespace, cachex.JSONCodec[channelAffinityOccupancyEntry]{})
	})
	return channelAffinityOccupancyCache
}

func getChannelAffinityLastBindCache() *cachex.HybridCache[channelAffinityLastBindRecord] {
	channelAffinityLastBindOnce.Do(func() {
		channelAffinityLastBindCache = newAffinityStoreCache(channelAffinityLastBindNamespace, cachex.JSONCodec[channelAffinityLastBindRecord]{})
	})
	return channelAffinityLastBindCache
}

// occupancyAddKeyFP 向渠道条目登记键指纹：条目缺失则新建，已含该指纹则仅续期整条目，
// 否则追加后按本次 TTL 写回（条目整体 TTL 按最近一次登记续期，04 文档 §4.1）。
// Redis 模式经 occupancyAddScript 原子执行读改写（登记路径无分段锁覆盖，非原子
// GET→SET 在并发下会丢失其它实例的成员写入）；内存模式由 T4 分段锁保证进程内原子性。
// 读改写失败返回 error 由调用方降级（SSOT 5.1.5）。
func occupancyAddKeyFP(channelID int, keyFP string, ttl time.Duration) error {
	cache := getChannelAffinityOccupancyCache()
	key := strconv.Itoa(channelID)
	if redisMode := common.RedisEnabled && common.RDB != nil; redisMode {
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		return occupancyAddScript.Run(ctx, common.RDB, []string{cache.FullKey(key)}, keyFP, ttl.Milliseconds()).Err()
	}
	entry, found, err := cache.Get(key)
	if err != nil {
		return fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", channelID, err)
	}
	if found {
		for _, fp := range entry.KeyFPs {
			if fp == keyFP {
				if err := cache.SetWithTTL(key, entry, ttl); err != nil {
					return fmt.Errorf("channel affinity occupancy renew failed: channel=%d err=%w", channelID, err)
				}
				occupancyMemExpireAt.Store(key, time.Now().Add(ttl))
				return nil
			}
		}
	}
	next := make([]string, 0, len(entry.KeyFPs)+1)
	next = append(next, entry.KeyFPs...)
	next = append(next, keyFP)
	if err := cache.SetWithTTL(key, channelAffinityOccupancyEntry{KeyFPs: next}, ttl); err != nil {
		return fmt.Errorf("channel affinity occupancy write failed: channel=%d err=%w", channelID, err)
	}
	occupancyMemExpireAt.Store(key, time.Now().Add(ttl))
	return nil
}

// occupancyRemoveKeyFP 从渠道条目移除键指纹：集合清空则删除条目，否则按剩余 TTL 写回
// （不续期）；成员或条目不存在时幂等 no-op。
func occupancyRemoveKeyFP(channelID int, keyFP string) error {
	cache := getChannelAffinityOccupancyCache()
	key := strconv.Itoa(channelID)
	entry, found, err := cache.Get(key)
	if err != nil {
		return fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", channelID, err)
	}
	if !found {
		occupancyMemExpireAt.Delete(key)
		return nil
	}
	kept := make([]string, 0, len(entry.KeyFPs))
	for _, fp := range entry.KeyFPs {
		if fp != keyFP {
			kept = append(kept, fp)
		}
	}
	if len(kept) == len(entry.KeyFPs) {
		return nil
	}
	if len(kept) == 0 {
		if _, err := cache.DeleteMany([]string{key}); err != nil {
			return fmt.Errorf("channel affinity occupancy delete failed: channel=%d err=%w", channelID, err)
		}
		occupancyMemExpireAt.Delete(key)
		return nil
	}
	remaining := occupancyRemainingTTL(cache, key)
	if err := cache.SetWithTTL(key, channelAffinityOccupancyEntry{KeyFPs: kept}, remaining); err != nil {
		return fmt.Errorf("channel affinity occupancy write failed: channel=%d err=%w", channelID, err)
	}
	return nil
}

// occupancyBindingCount 返回渠道条目的绑定键数与指纹列表（副本）。
// 键数含本键时由调用方自行判定空闲（SSOT 5.1.2 第2条）。
func occupancyBindingCount(channelID int) (int, []string, error) {
	entry, found, err := getChannelAffinityOccupancyCache().Get(strconv.Itoa(channelID))
	if err != nil {
		return 0, nil, fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", channelID, err)
	}
	if !found || len(entry.KeyFPs) == 0 {
		return 0, []string{}, nil
	}
	fps := make([]string, len(entry.KeyFPs))
	copy(fps, entry.KeyFPs)
	return len(fps), fps, nil
}

// occupancyRemainingTTL 返回条目剩余存活时间：Redis 模式查服务端 TTL，内存模式查
// 登记时记录的过期时刻；均查不到时回退默认 TTL（方向保守，条目多存活一个周期内收敛）。
func occupancyRemainingTTL(cache *cachex.HybridCache[channelAffinityOccupancyEntry], key string) time.Duration {
	if common.RedisEnabled && common.RDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		if ttl, err := common.RDB.TTL(ctx, cache.FullKey(key)).Result(); err == nil && ttl > 0 {
			return ttl
		}
		return occupancyFallbackTTL()
	}
	if v, ok := occupancyMemExpireAt.Load(key); ok {
		if expireAt, ok := v.(time.Time); ok {
			if remaining := time.Until(expireAt); remaining > 0 {
				return remaining
			}
		}
	}
	return occupancyFallbackTTL()
}

func occupancyFallbackTTL() time.Duration {
	seconds := 3600
	if setting := operation_setting.GetChannelAffinitySetting(); setting != nil && setting.DefaultTTLSeconds > 0 {
		seconds = setting.DefaultTTLSeconds
	}
	return time.Duration(seconds) * time.Second
}

// lastBindSet 写入最近绑定记录：键与正向缓存键同 suffix（命名空间不同），
// 值为渠道与绑定时间戳，TTL 由调用方传入（正向绑定两个周期，SSOT 5.2.4 规则2）。
func lastBindSet(cacheKeySuffix string, channelID int, ttl time.Duration) error {
	record := channelAffinityLastBindRecord{
		ChannelID: channelID,
		BoundAt:   common.GetTimestamp(),
	}
	if err := getChannelAffinityLastBindCache().SetWithTTL(cacheKeySuffix, record, ttl); err != nil {
		return fmt.Errorf("channel affinity last bind write failed: key=%s err=%w", cacheKeySuffix, err)
	}
	return nil
}

func lastBindGet(cacheKeySuffix string) (channelAffinityLastBindRecord, bool, error) {
	record, found, err := getChannelAffinityLastBindCache().Get(cacheKeySuffix)
	if err != nil {
		return channelAffinityLastBindRecord{}, false, fmt.Errorf("channel affinity last bind read failed: key=%s err=%w", cacheKeySuffix, err)
	}
	return record, found, nil
}

func lastBindDelete(cacheKeySuffix string) error {
	if _, err := getChannelAffinityLastBindCache().DeleteMany([]string{cacheKeySuffix}); err != nil {
		return fmt.Errorf("channel affinity last bind delete failed: key=%s err=%w", cacheKeySuffix, err)
	}
	return nil
}

// occupancyAddKeyFPAtomic 原子登记键指纹：Redis 模式经 occupancyAddScript 完成
// "读条目 -> 登记 -> 写回"的原子执行，与正向绑定的 SetNX（T4）配合实现跨实例先到先得
// （04 文档 §6.1）；内存模式回退 occupancyAddKeyFP，进程内由 T4 分段锁保证。
func occupancyAddKeyFPAtomic(channelID int, keyFP string, ttl time.Duration) error {
	if common.RedisEnabled && common.RDB != nil {
		cache := getChannelAffinityOccupancyCache()
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		fullKey := cache.FullKey(strconv.Itoa(channelID))
		return occupancyAddScript.Run(ctx, common.RDB, []string{fullKey}, keyFP, ttl.Milliseconds()).Err()
	}
	return occupancyAddKeyFP(channelID, keyFP, ttl)
}

// channelAffinityBindDecision 为独占绑定决策结果：选定渠道与绑定模式
// （exclusive=独占空闲渠道，shared=满载复用降级，SSOT 5.1.2 第2条）。
type channelAffinityBindDecision struct {
	ChannelID int
	Mode      string
}

// decideExclusiveBinding 为独占绑定决策纯函数（SSOT 5.1.2 第2条）：
// 空闲集合 = candidates 中 occupancyCounts[id]==0 的渠道。调用方须把本键既有绑定
// 从索引剔除后传入（对本键已登记的渠道键数减 1，本键既有绑定视为空闲）。
//   - 空闲集非空且 lastBindValid 且记录渠道在空闲集内：绑回原渠道，Mode=exclusive
//     （TTL 到期优先绑回，SSOT 5.1.4 规则4）
//   - 空闲集非空无有效记录：选定函数在空闲集内选定，Mode=exclusive
//   - 空闲集为空（满载）：全部候选按 occupancyCounts 升序取最少绑定数层，
//     Mode=shared（并列时选定函数在同层内选定，SSOT 5.1.4 规则3）
//   - candidates 为空：返回 ChannelID=0
//
// 选定函数由调用方注入（现有实现的均匀随机，偏离记录 SSOT 8.3 D1），保证本函数纯。
func decideExclusiveBinding(candidates []int, occupancyCounts map[int]int, lastBindChannelID int, lastBindValid bool, pickWeighted func([]int) int) channelAffinityBindDecision {
	if len(candidates) == 0 {
		return channelAffinityBindDecision{}
	}

	free := make([]int, 0, len(candidates))
	for _, id := range candidates {
		if occupancyCounts[id] == 0 {
			free = append(free, id)
		}
	}

	if len(free) > 0 {
		if lastBindValid {
			for _, id := range free {
				if id == lastBindChannelID {
					return channelAffinityBindDecision{ChannelID: id, Mode: affinityBindModeExclusive}
				}
			}
		}
		return channelAffinityBindDecision{ChannelID: pickWeighted(free), Mode: affinityBindModeExclusive}
	}

	// 满载降级：最少绑定数优先，选定函数在同层内选定。
	fewest := make([]int, 0, len(candidates))
	minCount := -1
	for _, id := range candidates {
		count := occupancyCounts[id]
		if minCount == -1 || count < minCount {
			minCount = count
			fewest = fewest[:0]
		}
		if count == minCount {
			fewest = append(fewest, id)
		}
	}
	return channelAffinityBindDecision{ChannelID: pickWeighted(fewest), Mode: affinityBindModeShared}
}

// channelAffinityDegradedReuseTotal 为满载复用降级累计计数器（进程内，重启清零，
// SSOT 5.3.3）。
var channelAffinityDegradedReuseTotal uint64

// GetChannelAffinityDegradedReuseTotal 返回累计降级次数，供缓存统计接口读取
// degraded_reuse_total 字段（SSOT 5.3.2 第3条）。
func GetChannelAffinityDegradedReuseTotal() uint64 {
	return atomic.LoadUint64(&channelAffinityDegradedReuseTotal)
}

// GetChannelAffinityExclusiveStats 按反向占用索引实时统计独占/复用绑定键数
// （SSOT 4.2.4 规则1）：遍历全部渠道条目，键数为 1 的渠道贡献 1 记独占、
// 键数大于 1 的渠道贡献其键数记复用，不区分规则来源（渠道全局口径）。
// 索引键遍历失败返回 (0, 0) 并记 SysError（03 文档 3.2 边界值：失败返回 0）。
// 成员残留滞后窗口属设计内（04 文档 §4.1），不做成员级精确过期。
func GetChannelAffinityExclusiveStats() (exclusive int, shared int) {
	cache := getChannelAffinityOccupancyCache()
	keys, err := cache.Keys()
	if err != nil {
		common.SysError(fmt.Sprintf("channel affinity occupancy list keys failed: err=%v", err))
		return 0, 0
	}
	for _, k := range keys {
		entry, found, err := cache.Get(k)
		if err != nil || !found {
			continue
		}
		n := len(entry.KeyFPs)
		if n == 1 {
			exclusive += 1
		} else if n > 1 {
			shared += n
		}
	}
	return exclusive, shared
}

// channelAffinityBindLocks 为独占占位分段锁：按 cacheKeySuffix 哈希取锁，
// 进程内串行化读-决策-写（内存模式先到先得；Redis 模式退化为本地收敛锁，
// 跨实例原子性由 Lua/SetNX 保证，04 文档 §6.1）。
var channelAffinityBindLocks [64]sync.Mutex

func channelAffinityBindLock(cacheKeySuffix string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(cacheKeySuffix))
	idx := h.Sum32() % uint32(len(channelAffinityBindLocks))
	return &channelAffinityBindLocks[idx]
}

// exclusiveAffinityTTL 由 meta 计算绑定 TTL：规则 TTL，缺省回退设置默认值再回退 3600。
func exclusiveAffinityTTL(meta channelAffinityMeta) time.Duration {
	seconds := meta.TTLSeconds
	if seconds <= 0 {
		if setting := operation_setting.GetChannelAffinitySetting(); setting != nil {
			seconds = setting.DefaultTTLSeconds
		}
	}
	if seconds <= 0 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}

// exclusivePickRandom 为 decideExclusiveBinding 注入的选定函数：候选集内均匀随机
// （现有选路为加权随机，独占空闲集无权重维度，偏离记录见 SSOT 8.3 D1）。
func exclusivePickRandom(ids []int) int {
	if len(ids) == 0 {
		return 0
	}
	if len(ids) == 1 {
		return ids[0]
	}
	return ids[common.GetRandomInt(len(ids))]
}

// exclusiveCandidateChannels 取候选集：普通分组单次查询；auto 分组经
// GetRequestAutoGroups 展开为多分组并集（每分组一次查询后合并去重），
// 用户分组取 context（与 distributor 的 auto 展开语义一致）。
func exclusiveCandidateChannels(c *gin.Context, meta channelAffinityMeta, usingGroup string, modelName string) []int {
	if usingGroup != "auto" {
		return model.GetEnabledChannelIDsForGroupModel(usingGroup, modelName, meta.RequestPath)
	}

	userGroup := ""
	if c != nil {
		userGroup = common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	}
	autoGroups := GetRequestAutoGroups(c, userGroup)
	seen := make(map[int]struct{})
	merged := make([]int, 0)
	for _, group := range autoGroups {
		for _, id := range model.GetEnabledChannelIDsForGroupModel(group, modelName, meta.RequestPath) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			merged = append(merged, id)
		}
	}
	return merged
}

// acquireExclusiveBinding 在亲和 miss 且规则启用独占时完成绑定决策与原子占位
// （SSOT 5.1.2 第2条）：读反向占用索引构造各候选渠道键数（本键既有绑定视为空闲，
// 对本键已登记的渠道键数减 1），分段锁内 double-check 正向缓存后决策并写三处
// （正向绑定、占用索引、最近绑定记录）。索引读写任一失败时回滚本次已写入的
// 占位并降级软亲和返回 (0, false)（SSOT 5.1.5）；锁内 double-check 命中即读
// 胜者结果使用同一渠道（SSOT 5.1.5）。
func acquireExclusiveBinding(c *gin.Context, meta channelAffinityMeta, usingGroup string, modelName string) (int, bool) {
	// 键指纹为空属提取异常：无法登记索引，直接降级软亲和，不阻塞请求（SSOT 5.1.5）。
	if meta.KeyFingerprint == "" {
		common.SysError(fmt.Sprintf("channel affinity exclusive bind skipped: empty key fingerprint, rule=%s key=%s", meta.RuleName, meta.CacheKey))
		return 0, false
	}

	candidates := exclusiveCandidateChannels(c, meta, usingGroup, modelName)
	if len(candidates) == 0 {
		return 0, false
	}

	cacheKeySuffix := strings.TrimPrefix(meta.CacheKey, channelAffinityCacheNamespace+":")

	// 读取本键现状：最近绑定记录（正向 miss 已由调用方保证）。
	lastBindRecord, lastBindFound, err := lastBindGet(cacheKeySuffix)
	if err != nil {
		common.SysError(fmt.Sprintf("channel affinity last bind read failed: channel=0 key_fp=%s err=%v", meta.KeyFingerprint, err))
		return 0, false
	}

	// 构造 occupancyCounts：本键指纹已登记的渠道键数减 1（本键既有绑定视为空闲）。
	occupancyCounts := make(map[int]int, len(candidates))
	for _, id := range candidates {
		count, fps, err := occupancyBindingCount(id)
		if err != nil {
			common.SysError(fmt.Sprintf("channel affinity occupancy read failed: channel=%d key_fp=%s err=%v", id, meta.KeyFingerprint, err))
			return 0, false
		}
		for _, fp := range fps {
			if fp == meta.KeyFingerprint && count > 0 {
				count--
				break
			}
		}
		occupancyCounts[id] = count
	}

	ttl := exclusiveAffinityTTL(meta)
	cache := getChannelAffinityCache()

	lock := channelAffinityBindLock(cacheKeySuffix)
	lock.Lock()
	defer lock.Unlock()

	// 锁内 double-check：胜者已写入正向绑定则直接读其结果（竞争失败语义，SSOT 5.1.5）。
	if winnerID, found, err := cache.Get(cacheKeySuffix); err == nil && found && winnerID > 0 {
		// 竞争失败同样记胜者渠道为首次占位渠道：该键重试切换成功时旧渠道索引
		// 才能正确迁移移除（缺标记时 old=0，旧渠道占位残留至 TTL）。
		if c != nil {
			c.Set(ginKeyChannelAffinityBoundChannel, winnerID)
		}
		return winnerID, true
	}

	decision := decideExclusiveBinding(candidates, occupancyCounts, lastBindRecord.ChannelID, lastBindFound, exclusivePickRandom)
	if decision.ChannelID <= 0 {
		return 0, false
	}

	redisMode := common.RedisEnabled && common.RDB != nil
	if redisMode {
		// Redis 模式：跨实例原子性由 SET NX PX 保证（04 文档 §6.1）。
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		ok, err := common.RDB.SetNX(ctx, cache.FullKey(cacheKeySuffix), strconv.Itoa(decision.ChannelID), ttl).Result()
		if err != nil {
			common.SysError(fmt.Sprintf("channel affinity exclusive setnx failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
			return 0, false
		}
		if !ok {
			// 跨实例竞争失败：读胜者结果使用同一渠道（SSOT 5.1.5），与内存模式锁内
			// double-check 语义一致：跳过降级计数、不重复占位三写（占位三写由胜者完成）。
			winnerID, found, err := cache.Get(cacheKeySuffix)
			if err != nil || !found || winnerID <= 0 {
				common.SysError(fmt.Sprintf("channel affinity exclusive loser confirm failed: channel=%d key_fp=%s found=%v err=%v", decision.ChannelID, meta.KeyFingerprint, found, err))
				return 0, false
			}
			// 同上：竞争失败记胜者渠道为首次占位渠道，供迁移移除与终态回滚使用。
			if c != nil {
				c.Set(ginKeyChannelAffinityBoundChannel, winnerID)
			}
			return winnerID, true
		}
	} else {
		// 内存模式：分段锁临界区内串行写入，进程内先到先得。
		if err := cache.SetWithTTL(cacheKeySuffix, decision.ChannelID, ttl); err != nil {
			common.SysError(fmt.Sprintf("channel affinity exclusive forward write failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
			return 0, false
		}
	}

	// 胜者收尾：登记反向占用索引与最近绑定记录（正向绑定已写入）。
	// 任一步失败即回滚正向绑定后降级软亲和：残留孤儿正向绑定无占位三写配套
	// （终态失败时无 boundChannel 标记可回滚），按独占未生效处理（SSOT 5.1.5），
	// 清除失败记 SysError，残留随 TTL 过期。
	if err := occupancyAddKeyFPAtomic(decision.ChannelID, meta.KeyFingerprint, ttl); err != nil {
		common.SysError(fmt.Sprintf("channel affinity occupancy register failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
		rollbackBindingPlacement(cache.FullKey(cacheKeySuffix), meta.KeyFingerprint, decision.ChannelID)
		return 0, false
	}
	if err := lastBindSet(cacheKeySuffix, decision.ChannelID, 2*ttl); err != nil {
		common.SysError(fmt.Sprintf("channel affinity last bind write failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
		rollbackBindingPlacement(cache.FullKey(cacheKeySuffix), meta.KeyFingerprint, decision.ChannelID)
		return 0, false
	}

	// 占位完成后读回正向缓存确认胜者结果。读回失败同样回滚：三处占位已全部写入，
	// 若放行降级软亲和，请求成功后 RecordChannelAffinity 会以 old=0 在另一渠道
	// 重复登记同一指纹，双渠道占位污染独占判定与统计（SSOT 5.1.5 任一步失败即回滚）。
	winnerID, found, err := cache.Get(cacheKeySuffix)
	if err != nil || !found || winnerID <= 0 {
		common.SysError(fmt.Sprintf("channel affinity exclusive winner confirm failed: channel=%d key_fp=%s found=%v err=%v", decision.ChannelID, meta.KeyFingerprint, found, err))
		rollbackBindingPlacement(cache.FullKey(cacheKeySuffix), meta.KeyFingerprint, decision.ChannelID)
		return 0, false
	}

	if decision.Mode == affinityBindModeShared {
		// 满载降级：计数器原子加 1，gin context 写降级标记（04 文档 §4.3 结构，
		// 供 MarkChannelAffinityUsed 合并进 admin_info），同步输出与请求关联的
		// LogWarn（SSOT 5.3.2 第1、2条）。nil context 下仅计数与日志（logHelper
		// 的 ctx.Value 在 nil *gin.Context 上会 panic，与下方 c==nil 守卫同口径）。
		atomic.AddUint64(&channelAffinityDegradedReuseTotal, 1)
		bindingCount := occupancyCounts[winnerID]
		if c != nil {
			logger.LogWarn(c, fmt.Sprintf(
				"channel affinity exclusive degrade to shared reuse: rule=%s key_fp=%s channel=%d channel_binding_count=%d",
				meta.RuleName, meta.KeyFingerprint, winnerID, bindingCount))
			c.Set(ginKeyChannelAffinityExclusiveDegrade, map[string]interface{}{
				"rule_name":             meta.RuleName,
				"key_fp":                meta.KeyFingerprint,
				"channel_id":            winnerID,
				"channel_binding_count": bindingCount,
			})
		}
	}

	// 记录本次请求首次占位渠道，供 RecordChannelAffinity 判定迁移、终态回滚判定切换。
	if c != nil {
		c.Set(ginKeyChannelAffinityBoundChannel, decision.ChannelID)
	}

	return winnerID, true
}

// registerBindingIndexes 绑定登记/迁移索引同步（SSOT 5.2.2 第1、2条）：
// oldChannelID>0 且不等于新渠道为失败切换迁移场景，先从旧渠道条目移除该键；随后
// 登记新渠道条目（同 TTL），并写入最近绑定记录（两周期）。old==new（亲和命中续期）
// 直接走登记续期，跳过移除：先删后加会在同渠道制造无锁空闲窗口，并发独占 miss
// 可借此误占。迁入豁免独占判定：直接登记，不检查新渠道占用（SSOT 5.1.2 第4条）。
// 任一步失败记 SysError（含渠道与键指纹），不影响请求，残留随 TTL 过期（SSOT 5.1.5）。
func registerBindingIndexes(cacheKeySuffix string, keyFP string, oldChannelID int, newChannelID int, ttl time.Duration) {
	if oldChannelID > 0 && oldChannelID != newChannelID {
		if err := occupancyRemoveKeyFP(oldChannelID, keyFP); err != nil {
			common.SysError(fmt.Sprintf("channel affinity occupancy migrate remove failed: channel=%d key_fp=%s err=%v", oldChannelID, keyFP, err))
		}
	}
	if err := occupancyAddKeyFP(newChannelID, keyFP, ttl); err != nil {
		common.SysError(fmt.Sprintf("channel affinity occupancy register failed: channel=%d key_fp=%s err=%v", newChannelID, keyFP, err))
	}
	if err := lastBindSet(cacheKeySuffix, newChannelID, 2*ttl); err != nil {
		common.SysError(fmt.Sprintf("channel affinity last bind write failed: channel=%d key_fp=%s err=%v", newChannelID, keyFP, err))
	}
}

// rollbackBindingPlacement 同批回滚占位三处（SSOT 5.1.2 第4条末）：
// 正向绑定（传 full key）、反向占用索引、最近绑定记录（lastBind 命名空间自行加前缀）。
// 失败记 SysError 后放行，残留随 TTL 过期（SSOT 5.1.5）。
func rollbackBindingPlacement(cacheKey string, keyFP string, channelID int) {
	cacheKeySuffix := strings.TrimPrefix(cacheKey, channelAffinityCacheNamespace+":")
	if _, err := getChannelAffinityCache().DeleteMany([]string{cacheKey}); err != nil {
		common.SysError(fmt.Sprintf("channel affinity rollback forward delete failed: channel=%d key_fp=%s err=%v", channelID, keyFP, err))
	}
	if err := occupancyRemoveKeyFP(channelID, keyFP); err != nil {
		common.SysError(fmt.Sprintf("channel affinity rollback occupancy remove failed: channel=%d key_fp=%s err=%v", channelID, keyFP, err))
	}
	if err := lastBindDelete(cacheKeySuffix); err != nil {
		common.SysError(fmt.Sprintf("channel affinity rollback last bind delete failed: channel=%d key_fp=%s err=%v", channelID, keyFP, err))
	}
}

// RollbackChannelAffinityOnFinalFailure 终态失败回滚入口（导出供 controller 调用，
// SSOT 5.1.2 第4条末）：无 affinity meta 直接返回；终态失败必然未发生成功切换
// （成功回写仅走 RecordChannelAffinity），一律回滚首次占位渠道三处，避免失败
// 占位把亲和键钉在失败渠道上直至 TTL 到期。回滚后在 gin context 标记终态失败，
// 供 RecordChannelAffinity 跳过重建（错误经 status_code_mapping 映射为 2xx 或
// WS 已升级 101 时，distributor 的 status<400 判定无法识别失败）。
func RollbackChannelAffinityOnFinalFailure(c *gin.Context) {
	if c == nil {
		return
	}
	meta, ok := getChannelAffinityMeta(c)
	if !ok {
		return
	}
	c.Set(ginKeyChannelAffinityFinalFailure, true)
	boundChannel := 0
	if v, exists := c.Get(ginKeyChannelAffinityBoundChannel); exists {
		if id, valid := v.(int); valid {
			boundChannel = id
		}
	}
	if boundChannel <= 0 {
		return
	}
	rollbackBindingPlacement(meta.CacheKey, meta.KeyFingerprint, boundChannel)
}

// affinityKeysDeleter 为命名空间整体清空所需的最小缓存接口（Keys + DeleteMany）。
type affinityKeysDeleter interface {
	Keys() ([]string, error)
	DeleteMany([]string) (map[string]bool, error)
}

// purgeAffinityNamespace 清空单个命名空间：列出全部键后批量删除。
func purgeAffinityNamespace(cache affinityKeysDeleter) error {
	keys, err := cache.Keys()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	_, err = cache.DeleteMany(keys)
	return err
}

// retryOnce 执行 fn，失败时重试一次（SSOT 5.2.5）。固定保留为独立函数：
// clearExclusiveRuntimeAll 与 clearExclusiveRuntimeByForwardKeys 两处调用，
// 表达清空路径的稳定重试语义。
func retryOnce(fn func() error) error {
	if err := fn(); err != nil {
		return fn()
	}
	return nil
}

// clearExclusiveRuntimeAll 整体清空 occupancy 与 lastBind 两命名空间
// （管理员全部清空路径，SSOT 5.2.2 第4条）：各自 Keys 后 DeleteMany，
// 失败重试一次，仍失败记 SysError（SSOT 5.2.5），残留随 TTL 过期收敛。
func clearExclusiveRuntimeAll() {
	if err := retryOnce(func() error {
		return purgeAffinityNamespace(getChannelAffinityOccupancyCache())
	}); err != nil {
		common.SysError(fmt.Sprintf("channel affinity occupancy clear all failed: err=%v", err))
	} else {
		// 条目已整体删除，内存过期时刻表同步清空。
		occupancyMemExpireAt.Range(func(key, _ any) bool {
			occupancyMemExpireAt.Delete(key)
			return true
		})
	}
	if err := retryOnce(func() error {
		return purgeAffinityNamespace(getChannelAffinityLastBindCache())
	}); err != nil {
		common.SysError(fmt.Sprintf("channel affinity last bind clear all failed: err=%v", err))
	}
}

// exclusiveKeyFingerprintsFromSuffix 由正向缓存键后缀推导候选键指纹：后缀按构成
// 分段（buildChannelAffinityCacheKeySuffix 为 rule/model/group/value 依次拼接），
// 末段起逐段取"该段及其后全部内容"为亲和值原文（值本身可含冒号），求指纹去重。
func exclusiveKeyFingerprintsFromSuffix(suffix string) []string {
	parts := strings.Split(suffix, ":")
	fps := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for i := 1; i < len(parts); i++ {
		fp := affinityFingerprint(strings.Join(parts[i:], ":"))
		if fp == "" {
			continue
		}
		if _, dup := seen[fp]; !dup {
			seen[fp] = struct{}{}
			fps = append(fps, fp)
		}
	}
	return fps
}

// removeExclusiveOccupancyBySuffix 成员级移除：候选指纹中第一个存在于该渠道条目
// 内的即命中并移除（命中即止），不整体删除渠道条目，避免误删其它规则同渠道的
// 键指纹（04 文档 §6.2）。推导失败的键（无候选段或条目内无命中）跳过并 SysLog
// 声明，残留依赖 TTL 收敛（SSOT 5.2.5）。
func removeExclusiveOccupancyBySuffix(channelID int, suffix string) {
	fps := exclusiveKeyFingerprintsFromSuffix(suffix)
	if len(fps) == 0 {
		common.SysLog(fmt.Sprintf("channel affinity clear skip: no fingerprint candidates derivable, key=%s", suffix))
		return
	}
	_, members, err := occupancyBindingCount(channelID)
	if err != nil {
		common.SysError(fmt.Sprintf("channel affinity occupancy read failed on clear: channel=%d key=%s err=%v", channelID, suffix, err))
		return
	}
	memberSet := make(map[string]struct{}, len(members))
	for _, fp := range members {
		memberSet[fp] = struct{}{}
	}
	for _, fp := range fps {
		if _, hit := memberSet[fp]; !hit {
			continue
		}
		if err := retryOnce(func() error {
			return occupancyRemoveKeyFP(channelID, fp)
		}); err != nil {
			common.SysError(fmt.Sprintf("channel affinity occupancy remove on clear failed: channel=%d key=%s err=%v", channelID, suffix, err))
		}
		return
	}
	common.SysLog(fmt.Sprintf("channel affinity clear skip: fingerprint not located in occupancy, channel=%d key=%s", channelID, suffix))
}

// clearExclusiveRuntimeByForwardKeys 按正向缓存键回放清理 occupancy 与 lastBind
// （管理员按规则清空路径，SSOT 5.2.2 第4条）：对每个 forwardKey（含命名空间前缀
// 的 full key）定位该键的绑定渠道，键指纹由正向键推导（成员级移除，04 文档 §6.2），
// 随后 lastBindDelete（去正向前缀）。渠道定位次序：boundChannels（调用方在正向
// DeleteByPrefix 前读出的 suffix→渠道映射，主路径；正向删除后自身已读不到值）、
// 正向缓存（调用方未删正向时兜底）、lastBind 记录（与正向同批写入、两周期 TTL，
// 值一致）。边界：渠道无法定位或指纹无法命中的键跳过 occupancy 移除并 SysLog
// 声明，残留依赖 TTL 收敛（SSOT 5.2.5）；清理失败重试一次，仍失败记 SysError。
func clearExclusiveRuntimeByForwardKeys(forwardKeys []string, boundChannels map[string]int) {
	if len(forwardKeys) == 0 {
		return
	}
	forward := getChannelAffinityCache()
	suffixes := make([]string, 0, len(forwardKeys))
	for _, fullKey := range forwardKeys {
		suffix, matched := strings.CutPrefix(fullKey, channelAffinityCacheNamespace+":")
		if !matched || suffix == "" {
			continue
		}
		suffixes = append(suffixes, suffix)

		channelID := 0
		if boundChannels != nil {
			channelID = boundChannels[suffix]
		}
		if channelID <= 0 {
			if v, found, err := forward.Get(suffix); err != nil {
				common.SysError(fmt.Sprintf("channel affinity forward read failed on clear: key=%s err=%v", fullKey, err))
				continue
			} else if found {
				channelID = v
			}
		}
		if channelID <= 0 {
			// 正向已被调用方删除且调用方未传渠道：回退读 lastBind 的绑定渠道
			// （与正向同批写入，值一致）。
			record, lbFound, lbErr := lastBindGet(suffix)
			if lbErr != nil {
				common.SysError(fmt.Sprintf("channel affinity last bind read failed on clear: key=%s err=%v", fullKey, lbErr))
				continue
			}
			if !lbFound || record.ChannelID <= 0 {
				common.SysLog(fmt.Sprintf("channel affinity clear skip: channel not resolvable, key=%s", suffix))
				continue
			}
			channelID = record.ChannelID
		}
		removeExclusiveOccupancyBySuffix(channelID, suffix)
	}
	if len(suffixes) > 0 {
		if err := retryOnce(func() error {
			_, err := getChannelAffinityLastBindCache().DeleteMany(suffixes)
			return err
		}); err != nil {
			common.SysError(fmt.Sprintf("channel affinity last bind clear by keys failed: err=%v", err))
		}
	}
}

// clearExclusiveRuntimeByRuleNamePrefix 按规则名前缀清空正向缓存并回放清理
// occupancy 与 lastBind（三批同清，SSOT 5.2.2 第4条）：DeleteByPrefix 前先收集
// 该前缀下全部正向键并读出绑定渠道（渠道值必须在删除前读取：删除后无法再定位
// 该键占用的渠道；boundChannels 的键与 clearExclusiveRuntimeByForwardKeys 的
// suffix 同口径，full key 去正向命名空间前缀），DeleteByPrefix 失败重试一次
// （SSOT 5.2.5），仍失败返回 error 且不回放（正向可能未删，回放会造成正反不一致，
// 残留随 TTL 收敛）。调用方两种：ClearChannelAffinityCacheByRuleName 复用本路径
// 并透传返回值（跳过其 include_rule_name 校验的开关切换路径同理，清空对未启用
// include_rule_name 的规则同样生效）；HandleChannelAffinityRulesUpdate 对返回的
// error 仅 SysError 不中断保存（保存以配置落库为准，03 文档 3.1 第3条）。
func clearExclusiveRuntimeByRuleNamePrefix(ruleName string) (int, error) {
	cache := getChannelAffinityCache()
	fullPrefix := channelAffinityCacheNamespace + ":" + ruleName + ":"
	ruleKeys := make([]string, 0)
	boundChannels := make(map[string]int)
	if keys, err := cache.Keys(); err != nil {
		common.SysError(fmt.Sprintf("channel affinity cache list keys failed: err=%v", err))
	} else {
		for _, k := range keys {
			if !strings.HasPrefix(k, fullPrefix) {
				continue
			}
			ruleKeys = append(ruleKeys, k)
			suffix := strings.TrimPrefix(k, channelAffinityCacheNamespace+":")
			if channelID, found, err := cache.Get(k); err == nil && found && channelID > 0 {
				boundChannels[suffix] = channelID
			}
		}
	}
	var deleted int
	if err := retryOnce(func() error {
		d, err := cache.DeleteByPrefix(ruleName)
		if err == nil {
			deleted = d
		}
		return err
	}); err != nil {
		return 0, err
	}
	clearExclusiveRuntimeByForwardKeys(ruleKeys, boundChannels)
	return deleted, nil
}

// HandleChannelAffinityRulesUpdate 规则数组保存后的开关联动清空入口
// （SSOT 4.1.4 规则1、5.2.2 第4条）：解析两侧规则数组（失败静默返回，保存主流程
// 不受影响），按 name 对齐逐规则比对 ExclusiveBind，对发生变化的规则清空其三命名
// space 运行时数据。旧规则启用 include_rule_name 时按规则名前缀清除；未启用且
// 键构成（IncludeModelName/IncludeUsingGroup 组合 + 亲和值）无规则名维度、无法
// 回放可区分前缀时，退化为正向缓存整体清空 + clearExclusiveRuntimeAll，并 SysLog
// 声明该退化（03 文档 3.1 服务端处理逻辑第2条工程边界，清除范围扩大属工程兜底）。
func HandleChannelAffinityRulesUpdate(oldRulesJSON string, newRulesJSON string) {
	var oldRules, newRules []operation_setting.ChannelAffinityRule
	if err := common.Unmarshal([]byte(oldRulesJSON), &oldRules); err != nil {
		return
	}
	if err := common.Unmarshal([]byte(newRulesJSON), &newRules); err != nil {
		return
	}

	newByName := make(map[string]bool, len(newRules))
	for _, rule := range newRules {
		newByName[strings.TrimSpace(rule.Name)] = rule.ExclusiveBind
	}

	for _, oldRule := range oldRules {
		ruleName := strings.TrimSpace(oldRule.Name)
		if ruleName == "" {
			continue
		}
		newExclusive, ok := newByName[ruleName]
		if ok && newExclusive == oldRule.ExclusiveBind {
			continue
		}
		// 规则被删除（!ok）或改名后旧名残留：与开关切换同样按旧规则清空，
		// 避免已删规则的键继续占用渠道至 TTL（改名场景旧前缀残留会双计占用）。
		if !ok && !oldRule.ExclusiveBind {
			// 旧规则本就未启用独占：无独占语义数据需要联动，跳过。
			continue
		}
		if oldRule.IncludeRuleName {
			// 清空失败仅 SysError 不中断保存（保存以配置落库为准，03 文档 3.1 第3条）。
			if _, err := clearExclusiveRuntimeByRuleNamePrefix(ruleName); err != nil {
				common.SysError(fmt.Sprintf("channel affinity forward clear by rule failed: rule=%s err=%v", ruleName, err))
			}
			continue
		}
		if oldRule.IncludeModelName || oldRule.IncludeUsingGroup {
			// 旧键构成含 model/group 段但无规则名段：组合同样无规则维度，
			// 与纯亲和值键一致无法回放出可区分前缀，统一走整体清空兜底。
			common.SysLog(fmt.Sprintf(
				"channel affinity rules update clear fallback to full clear: rule=%s key layout (model=%t group=%t rule_name=false) has no rule-scoped prefix",
				ruleName, oldRule.IncludeModelName, oldRule.IncludeUsingGroup))
		} else {
			common.SysLog(fmt.Sprintf(
				"channel affinity rules update clear fallback to full clear: rule=%s key layout is raw affinity value only",
				ruleName))
		}
		if err := retryOnce(func() error {
			return getChannelAffinityCache().Purge()
		}); err != nil {
			common.SysError(fmt.Sprintf("channel affinity forward clear all failed: err=%v", err))
		}
		clearExclusiveRuntimeAll()
	}
}

func init() {
	operation_setting.OnRulesExclusiveBindChanged = HandleChannelAffinityRulesUpdate
}
