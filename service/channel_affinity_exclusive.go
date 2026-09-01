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

func getChannelAffinityOccupancyCache() *cachex.HybridCache[channelAffinityOccupancyEntry] {
	channelAffinityOccupancyOnce.Do(func() {
		setting := operation_setting.GetChannelAffinitySetting()
		capacity := setting.MaxEntries
		if capacity <= 0 {
			capacity = 100_000
		}
		defaultTTLSeconds := setting.DefaultTTLSeconds
		if defaultTTLSeconds <= 0 {
			defaultTTLSeconds = 3600
		}

		channelAffinityOccupancyCache = cachex.NewHybridCache[channelAffinityOccupancyEntry](cachex.HybridCacheConfig[channelAffinityOccupancyEntry]{
			Namespace: cachex.Namespace(channelAffinityOccupancyNamespace),
			Redis:     common.RDB,
			RedisEnabled: func() bool {
				return common.RedisEnabled && common.RDB != nil
			},
			RedisCodec: cachex.JSONCodec[channelAffinityOccupancyEntry]{},
			Memory: func() *hot.HotCache[string, channelAffinityOccupancyEntry] {
				return hot.NewHotCache[string, channelAffinityOccupancyEntry](hot.LRU, capacity).
					WithTTL(time.Duration(defaultTTLSeconds) * time.Second).
					WithJanitor().
					Build()
			},
		})
	})
	return channelAffinityOccupancyCache
}

func getChannelAffinityLastBindCache() *cachex.HybridCache[channelAffinityLastBindRecord] {
	channelAffinityLastBindOnce.Do(func() {
		setting := operation_setting.GetChannelAffinitySetting()
		capacity := setting.MaxEntries
		if capacity <= 0 {
			capacity = 100_000
		}
		defaultTTLSeconds := setting.DefaultTTLSeconds
		if defaultTTLSeconds <= 0 {
			defaultTTLSeconds = 3600
		}

		channelAffinityLastBindCache = cachex.NewHybridCache[channelAffinityLastBindRecord](cachex.HybridCacheConfig[channelAffinityLastBindRecord]{
			Namespace: cachex.Namespace(channelAffinityLastBindNamespace),
			Redis:     common.RDB,
			RedisEnabled: func() bool {
				return common.RedisEnabled && common.RDB != nil
			},
			RedisCodec: cachex.JSONCodec[channelAffinityLastBindRecord]{},
			Memory: func() *hot.HotCache[string, channelAffinityLastBindRecord] {
				return hot.NewHotCache[string, channelAffinityLastBindRecord](hot.LRU, capacity).
					WithTTL(time.Duration(defaultTTLSeconds) * time.Second).
					WithJanitor().
					Build()
			},
		})
	})
	return channelAffinityLastBindCache
}

// occupancyAddKeyFP 向渠道条目登记键指纹：条目缺失则新建，已含该指纹则仅续期整条目，
// 否则追加后按本次 TTL 写回（条目整体 TTL 按最近一次登记续期，04 文档 §4.1）。
// 读改写失败返回 error 由调用方降级（SSOT 5.1.5）；内存模式的进程内原子性由 T4 分段锁保证。
func occupancyAddKeyFP(channelID int, keyFP string, ttl time.Duration) error {
	cache := getChannelAffinityOccupancyCache()
	key := strconv.Itoa(channelID)
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
//   - 空闲集非空无有效记录：pickWeighted 在空闲集内选定，Mode=exclusive
//   - 空闲集为空（满载）：全部候选按 occupancyCounts 升序取最少绑定数层，
//     Mode=shared（并列时 pickWeighted 在同层内选定，SSOT 5.1.4 规则3）
//   - candidates 为空：返回 ChannelID=0
//
// pickWeighted 由调用方注入（现有加权随机逻辑或首个空闲渠道），保证本函数纯。
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

	// 满载降级：最少绑定数优先，pickWeighted 在同层内选定。
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
		if setting := operation_setting.GetChannelAffinitySetting(); setting != nil && setting.DefaultTTLSeconds > 0 {
			seconds = setting.DefaultTTLSeconds
		}
	}
	if seconds <= 0 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}

// exclusivePickWeighted 为 decideExclusiveBinding 注入的选定函数：候选集内等权随机
// （现有选路为加权随机，独占空闲集无权重维度，按均匀随机在集内选定）。
func exclusivePickWeighted(ids []int) int {
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
// （正向绑定、占用索引、最近绑定记录）。索引读写任一失败时降级软亲和返回 (0, false)
// （SSOT 5.1.5）；锁内 double-check 命中即读胜者结果使用同一渠道（SSOT 5.1.5）。
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
		return winnerID, true
	}

	decision := decideExclusiveBinding(candidates, occupancyCounts, lastBindRecord.ChannelID, lastBindFound, exclusivePickWeighted)
	if decision.ChannelID <= 0 {
		return 0, false
	}

	redisMode := common.RedisEnabled && common.RDB != nil
	won := true
	if redisMode {
		// Redis 模式：跨实例原子性由 SET NX PX 保证，败者重读胜者渠道（04 文档 §6.1）。
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		ok, err := common.RDB.SetNX(ctx, cache.FullKey(cacheKeySuffix), strconv.Itoa(decision.ChannelID), ttl).Result()
		if err != nil {
			common.SysError(fmt.Sprintf("channel affinity exclusive setnx failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
			return 0, false
		}
		if !ok {
			won = false
		}
	} else {
		// 内存模式：分段锁临界区内串行写入，进程内先到先得。
		if err := cache.SetWithTTL(cacheKeySuffix, decision.ChannelID, ttl); err != nil {
			common.SysError(fmt.Sprintf("channel affinity exclusive forward write failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
			return 0, false
		}
	}

	if won {
		if err := occupancyAddKeyFPAtomic(decision.ChannelID, meta.KeyFingerprint, ttl); err != nil {
			common.SysError(fmt.Sprintf("channel affinity occupancy register failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
			return 0, false
		}
		if err := lastBindSet(cacheKeySuffix, decision.ChannelID, 2*ttl); err != nil {
			common.SysError(fmt.Sprintf("channel affinity last bind write failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
			return 0, false
		}
	}

	// 占位完成后无论模式都在锁内 double-check 正向缓存确认胜者结果。
	winnerID, found, err := cache.Get(cacheKeySuffix)
	if err != nil || !found || winnerID <= 0 {
		common.SysError(fmt.Sprintf("channel affinity exclusive winner confirm failed: channel=%d key_fp=%s found=%v err=%v", decision.ChannelID, meta.KeyFingerprint, found, err))
		return 0, false
	}

	if decision.Mode == affinityBindModeShared {
		// 满载降级：计数器原子加 1，gin context 写降级标记（04 文档 §4.3 结构，
		// 供 MarkChannelAffinityUsed 合并进 admin_info，审计落点 T8 完善）。
		atomic.AddUint64(&channelAffinityDegradedReuseTotal, 1)
		if c != nil {
			c.Set(ginKeyChannelAffinityExclusiveDegrade, map[string]interface{}{
				"rule_name":             meta.RuleName,
				"key_fp":                meta.KeyFingerprint,
				"channel_id":            winnerID,
				"channel_binding_count": occupancyCounts[winnerID],
			})
		}
	}

	return winnerID, true
}
