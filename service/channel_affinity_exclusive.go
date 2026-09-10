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
// v2：成员从数组升级为 指纹 -> 成员过期时刻（Unix 毫秒）的 map，读路径惰性剔除
// 过期成员。旧数组格式（v1，成员无独立过期）经 occupancyEntryCodec 读入，
// 保证升级瞬间不丢占用。
type channelAffinityOccupancyEntry struct {
	KeyFPs map[string]int64 `json:"key_fps"`
}

// occupancyEntryMembers 返回条目的存活成员（惰性剔除已过期成员，v1 成员视为存活）。
func occupancyEntryMembers(entry channelAffinityOccupancyEntry, nowMs int64) []string {
	members := make([]string, 0, len(entry.KeyFPs))
	for fp, expireAt := range entry.KeyFPs {
		if expireAt > 0 && expireAt <= nowMs {
			continue
		}
		members = append(members, fp)
	}
	return members
}

// occupancyEntryCodec 为 occupancy 条目的 JSON 编解码：编码固定输出 v2 map 格式；
// 解码兼容 v1 数组格式（成员转 map、过期时刻 0 表示存活，随下次写回重写为 v2），
// 升级瞬间既有占用不丢、已漂移残留仍按成员过期语义收敛。
type occupancyEntryCodec struct{}

func (occupancyEntryCodec) Encode(v channelAffinityOccupancyEntry) (string, error) {
	b, err := common.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (occupancyEntryCodec) Decode(s string) (channelAffinityOccupancyEntry, error) {
	var entry channelAffinityOccupancyEntry
	if err := common.Unmarshal([]byte(s), &entry); err == nil {
		return entry, nil
	}
	// v1 数组格式：key_fps 为字符串数组。
	var legacy struct {
		KeyFPs []string `json:"key_fps"`
	}
	if err := common.Unmarshal([]byte(s), &legacy); err != nil {
		return channelAffinityOccupancyEntry{}, err
	}
	members := make(map[string]int64, len(legacy.KeyFPs))
	for _, fp := range legacy.KeyFPs {
		members[fp] = 0 // 0 = 存活（无成员级过期），首次写回时重写为 v2
	}
	return channelAffinityOccupancyEntry{KeyFPs: members}, nil
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

// occupancyEntryPttlLua 为条目写回 PX 的公共计算：取本次 TTL 与现有条目剩余寿命
// （PTTL）的较大者，保证登记只延长条目寿命、绝不截断——短 TTL 成员后到时若把
// 条目 PX 缩到自身 TTL，长 TTL 成员会随条目整体消失，占用丢失被新键 claim 形成
// 静默双占。拼接进各脚本头部使用。
const occupancyEntryPttlLua = `
local function entryPttlMs(key, ttl_ms, now_ms)
  local pttl = redis.call('PTTL', key)
  if pttl > ttl_ms then
    return pttl
  end
  return ttl_ms
end
`

// occupancyAddScript 在 Redis 模式下原子完成"读条目 -> 登记成员 -> 写回"：
// KEYS[1] 为 occupancy 条目全键；ARGV[1] 键指纹，ARGV[2] 成员与条目 TTL 毫秒，
// ARGV[3] 当前时刻毫秒（客户端传入，避免脚本内 TIME 的不确定性），
// ARGV[4] v1 数组格式成员的回退过期毫秒（升级兼容）。
// 成员结构为 指纹->过期时刻 的 map：读取时惰性剔除已过期成员，登记成员写入
// now+ttl。v1 数组格式成员按 now+回退 TTL 重写为 v2。条目 PX 取本次 TTL 与
// 现有条目剩余寿命的较大者（entryPttlMs）：短 TTL 成员后到时不得截断条目，
// 否则长 TTL 成员随条目整体消失、占用丢失被新键 claim 形成静默双占。
var occupancyAddScript = redis.NewScript(occupancyEntryPttlLua + `
local key = KEYS[1]
local fp = ARGV[1]
local ttl_ms = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local legacy_ms = tonumber(ARGV[4])
local fps = {}
local raw = redis.call('GET', key)
if raw and raw ~= '' then
  local ok, obj = pcall(cjson.decode, raw)
  if ok and type(obj) == 'table' and type(obj.key_fps) == 'table' then
    local members = obj.key_fps
    if members[1] ~= nil then
      for _, m in ipairs(members) do
        fps[m] = now_ms + legacy_ms
      end
    else
      for m, exp in pairs(members) do
        if exp == 0 or exp > now_ms then
          fps[m] = exp
        end
      end
    end
  end
end
fps[fp] = now_ms + ttl_ms
redis.call('SET', key, cjson.encode({key_fps = fps}), 'PX', entryPttlMs(key, ttl_ms, now_ms))
return 1
`)

// occupancyClaimExclusiveScript 为独占条件占位的 Redis 实现（D3 竞态根治）：
// KEYS[1] 为 occupancy 条目全键，ARGV 同 occupancyAddScript。
// 读取时惰性剔除过期成员；条件语义：存活成员集为空（或仅本键）时登记并返回 1；
// 已含其它存活指纹时不动条目、返回存活键数（负数约定为冲突）。条目 PX 与登记
// 脚本同口径取 max(本次 TTL, 现有条目剩余寿命)，短 TTL 重入不得截断条目。
var occupancyClaimExclusiveScript = redis.NewScript(occupancyEntryPttlLua + `
local key = KEYS[1]
local fp = ARGV[1]
local ttl_ms = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local legacy_ms = tonumber(ARGV[4])
local fps = {}
local raw = redis.call('GET', key)
if raw and raw ~= '' then
  local ok, obj = pcall(cjson.decode, raw)
  if ok and type(obj) == 'table' and type(obj.key_fps) == 'table' then
    local members = obj.key_fps
    if members[1] ~= nil then
      for _, m in ipairs(members) do
        fps[m] = now_ms + legacy_ms
      end
    else
      for m, exp in pairs(members) do
        if exp == 0 or exp > now_ms then
          fps[m] = exp
        end
      end
    end
  end
end
if fps[fp] ~= nil then
  fps[fp] = now_ms + ttl_ms
  redis.call('SET', key, cjson.encode({key_fps = fps}), 'PX', entryPttlMs(key, ttl_ms, now_ms))
  return 1
end
local n = 0
for _ in pairs(fps) do n = n + 1 end
if n > 0 then
  return 0 - n
end
fps[fp] = now_ms + ttl_ms
redis.call('SET', key, cjson.encode({key_fps = fps}), 'PX', entryPttlMs(key, ttl_ms, now_ms))
return 1
`)

// occupancyRemoveScript 为移除路径的 Redis 原子实现：KEYS[1] 为 occupancy 条目全键，
// ARGV[1] 为键指纹，ARGV[2] 为当前时刻毫秒，ARGV[3] 为回退 TTL 毫秒（条目 PTTL
// 异常或 v1 成员重写时使用）。读条目 -> 惰性剔除过期成员 -> 移除目标成员 ->
// 空则 DEL / 否则按剩余 TTL（PTTL，不续期）写回，成员不在条目内时幂等 no-op。
var occupancyRemoveScript = redis.NewScript(`
local key = KEYS[1]
local fp = ARGV[1]
local now_ms = tonumber(ARGV[2])
local fallback_ms = tonumber(ARGV[3])
local raw = redis.call('GET', key)
if not raw or raw == '' then
  return 1
end
local ok, obj = pcall(cjson.decode, raw)
if not ok or type(obj) ~= 'table' or type(obj.key_fps) ~= 'table' then
  return 1
end
local members = obj.key_fps
local fps = {}
local changed = false
if members[1] ~= nil then
  for _, m in ipairs(members) do
    if m ~= fp then
      fps[m] = now_ms + fallback_ms
    end
  end
  changed = true
else
  for m, exp in pairs(members) do
    if m == fp then
      changed = true
    elseif exp == 0 or exp > now_ms then
      fps[m] = exp
    else
      changed = true
    end
  end
end
if not changed then
  return 1
end
local n = 0
for _ in pairs(fps) do n = n + 1 end
if n == 0 then
  redis.call('DEL', key)
  return 1
end
local pttl = redis.call('PTTL', key)
if pttl <= 0 then
  pttl = fallback_ms
end
redis.call('SET', key, cjson.encode({key_fps = fps}), 'PX', pttl)
return 1
`)

// occupancyPlaceSharedScript 为满载复用降级的服务端原子最小落位（跨实例串行）：
// KEYS[1..N] 为全部候选渠道的 occupancy 条目全键（顺序与调用方 candidates 一致），
// ARGV[1] 键指纹，ARGV[2] 成员与条目 TTL 毫秒，ARGV[3] 当前时刻毫秒，ARGV[4] v1
// 成员回退过期毫秒，ARGV[5] 随机起点（同层内均衡打散）。
// 单次脚本执行内读取全部候选的实时存活成员数（剔除本键指纹），在最少绑定数层内
// 按随机起点选定渠道并登记（成员合并/过期剔除/v1 重写/条目 PX 只延长不截断，
// 与登记脚本同语义）。返回 {候选下标(0基), 选定前该渠道其它键数}。
// 决策与登记同脚本原子完成，根治"多键并发共享陈旧快照、各自在失效的最少绑定
// 层无条件追加"的堆积（生产症状：渠道大量空闲时多键集中复用同一渠道）。
// 选中前键数为 0 表示实际存在空闲渠道（快照误判满载），调用方按独占处理免降级。
// 注：脚本跨多键访问，要求 standalone Redis（项目 common.RDB 为单连接客户端）。
var occupancyPlaceSharedScript = redis.NewScript(occupancyEntryPttlLua + `
local fp = ARGV[1]
local ttl_ms = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local legacy_ms = tonumber(ARGV[4])
local start_idx = tonumber(ARGV[5])

local function liveMembers(raw)
  local fps = {}
  if not raw or raw == '' then
    return fps
  end
  local ok, obj = pcall(cjson.decode, raw)
  if not ok or type(obj) ~= 'table' or type(obj.key_fps) ~= 'table' then
    return fps
  end
  local members = obj.key_fps
  if members[1] ~= nil then
    for _, m in ipairs(members) do
      fps[m] = 0
    end
  else
    for m, exp in pairs(members) do
      if exp == 0 or exp > now_ms then
        fps[m] = exp
      end
    end
  end
  return fps
end

local counts = {}
local min_n = -1
for i = 1, #KEYS do
  local fps = liveMembers(redis.call('GET', KEYS[i]))
  local n = 0
  for m, _ in pairs(fps) do
    if m ~= fp then
      n = n + 1
    end
  end
  counts[i] = n
  if min_n < 0 or n < min_n then
    min_n = n
  end
end

local tier = {}
for i = 1, #KEYS do
  if counts[i] == min_n then
    tier[#tier + 1] = i
  end
end
local pick = tier[(start_idx % #tier) + 1]

local key = KEYS[pick]
local fps = liveMembers(redis.call('GET', key))
for m, exp in pairs(fps) do
  if exp == 0 then
    fps[m] = now_ms + legacy_ms
  end
end
fps[fp] = now_ms + ttl_ms
redis.call('SET', key, cjson.encode({key_fps = fps}), 'PX', entryPttlMs(key, ttl_ms, now_ms))
return {pick - 1, min_n}
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
		channelAffinityOccupancyCache = newAffinityStoreCache(channelAffinityOccupancyNamespace, occupancyEntryCodec{})
	})
	return channelAffinityOccupancyCache
}

func getChannelAffinityLastBindCache() *cachex.HybridCache[channelAffinityLastBindRecord] {
	channelAffinityLastBindOnce.Do(func() {
		channelAffinityLastBindCache = newAffinityStoreCache(channelAffinityLastBindNamespace, cachex.JSONCodec[channelAffinityLastBindRecord]{})
	})
	return channelAffinityLastBindCache
}

// occupancyAddKeyFP 向渠道条目登记键指纹：登记成员按本次 TTL 写入成员级过期时刻，
// 条目 TTL 取本次 TTL 与既有条目剩余寿命的较大者（只延长不截断，见
// occupancyEntryPttlLua）。存活成员集已含其它成员时一并惰性剔除过期成员后写回。
// 渠道条目分段锁内执行，跨键并发的读-改-写不丢失成员（Redis 模式脚本原子，
// 锁退化为同实例收敛；内存模式锁是唯一的原子性保证）。读改写失败返回 error
// 由调用方降级（SSOT 5.1.5）。
func occupancyAddKeyFP(channelID int, keyFP string, ttl time.Duration) error {
	cache := getChannelAffinityOccupancyCache()
	key := strconv.Itoa(channelID)
	lock := occupancyEntryLock(channelID)
	lock.Lock()
	defer lock.Unlock()
	if common.RedisEnabled && common.RDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		return occupancyAddScript.Run(ctx, common.RDB, []string{cache.FullKey(key)},
			keyFP, ttl.Milliseconds(), time.Now().UnixMilli(), occupancyLegacyMemberTTL().Milliseconds()).Err()
	}
	entry, found, err := cache.Get(key)
	if err != nil {
		return fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", channelID, err)
	}
	nowMs := time.Now().UnixMilli()
	next := map[string]int64{}
	if found {
		for _, fp := range occupancyEntryMembers(entry, nowMs) {
			next[fp] = entry.KeyFPs[fp]
		}
	}
	next[keyFP] = nowMs + ttl.Milliseconds()
	entryTTL := ttl
	if remaining := occupancyRemainingTTL(cache, key); remaining > entryTTL {
		entryTTL = remaining
	}
	if err := cache.SetWithTTL(key, channelAffinityOccupancyEntry{KeyFPs: next}, entryTTL); err != nil {
		return fmt.Errorf("channel affinity occupancy write failed: channel=%d err=%w", channelID, err)
	}
	occupancyMemExpireAt.Store(key, time.Now().Add(entryTTL))
	return nil
}

// occupancyLegacyMemberTTL 为 v1 数组格式成员重写为 v2 时的回退成员寿命：
// 取默认 TTL（条目级），保证升级重写不改变成员的实质占用时长量级。
func occupancyLegacyMemberTTL() time.Duration {
	return occupancyFallbackTTL()
}

// occupancyRemoveKeyFP 从渠道条目移除键指纹：读取时惰性剔除过期成员，集合清空则
// 删除条目，否则按剩余 TTL 写回（不续期）；成员或条目不存在时幂等 no-op。
// Redis 模式经 occupancyRemoveScript 原子执行（与登记/占位脚本互斥，回写不覆盖
// 并发追加的成员）；内存模式在渠道条目分段锁内读改写，与登记共用该锁，
// 并发登记与移除对同一渠道条目互斥。
func occupancyRemoveKeyFP(channelID int, keyFP string) error {
	cache := getChannelAffinityOccupancyCache()
	key := strconv.Itoa(channelID)
	if common.RedisEnabled && common.RDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		fullKey := cache.FullKey(key)
		return occupancyRemoveScript.Run(ctx, common.RDB, []string{fullKey},
			keyFP, time.Now().UnixMilli(), occupancyFallbackTTL().Milliseconds()).Err()
	}
	lock := occupancyEntryLock(channelID)
	lock.Lock()
	defer lock.Unlock()
	entry, found, err := cache.Get(key)
	if err != nil {
		return fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", channelID, err)
	}
	if !found {
		occupancyMemExpireAt.Delete(key)
		return nil
	}
	nowMs := time.Now().UnixMilli()
	next := map[string]int64{}
	removed := false
	for fp, expireAt := range entry.KeyFPs {
		if fp == keyFP {
			removed = true
			continue
		}
		if expireAt > 0 && expireAt <= nowMs {
			removed = true // 过期成员随移除一并清理
			continue
		}
		next[fp] = expireAt
	}
	if !removed {
		return nil
	}
	if len(next) == 0 {
		if _, err := cache.DeleteMany([]string{key}); err != nil {
			return fmt.Errorf("channel affinity occupancy delete failed: channel=%d err=%w", channelID, err)
		}
		occupancyMemExpireAt.Delete(key)
		return nil
	}
	remaining := occupancyRemainingTTL(cache, key)
	if err := cache.SetWithTTL(key, channelAffinityOccupancyEntry{KeyFPs: next}, remaining); err != nil {
		return fmt.Errorf("channel affinity occupancy write failed: channel=%d err=%w", channelID, err)
	}
	return nil
}

// occupancyBindingCount 返回渠道条目的存活成员数与指纹列表（副本，惰性剔除已
// 过期成员）。键数含本键时由调用方自行判定空闲（SSOT 5.1.2 第2条）。
func occupancyBindingCount(channelID int) (int, []string, error) {
	entry, found, err := getChannelAffinityOccupancyCache().Get(strconv.Itoa(channelID))
	if err != nil {
		return 0, nil, fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", channelID, err)
	}
	if !found || len(entry.KeyFPs) == 0 {
		return 0, []string{}, nil
	}
	fps := occupancyEntryMembers(entry, time.Now().UnixMilli())
	return len(fps), fps, nil
}

// occupancyPlacement 为满载复用降级的原子最小落位结果：ChannelID 为选定渠道，
// HolderCount 为选中前该渠道的其它键数（0 表示服务端复核发现空闲，按独占处理）。
type occupancyPlacement struct {
	ChannelID   int
	HolderCount int
}

// occupancyPlaceSharedLeast 满载降级的原子最小落位：在全部候选渠道的实时占用中
// 选最少绑定数层（同层按调用方给定的随机起点打散），并把本键指纹登记到选定渠道，
// 决策与登记一次完成。Redis 模式经 occupancyPlaceSharedScript 服务端原子执行
//（跨实例串行，根治并发键共享陈旧快照集中挤入同一渠道的堆积）；内存模式在
// placement 全局锁内重读实时占用后决策登记（进程内串行，等价语义）。
// 随机起点由调用方传入以保持本函数可测（注入式随机，与 pickWeighted 同思路）。
func occupancyPlaceSharedLeast(candidates []int, keyFP string, ttl time.Duration, startIdx int) (occupancyPlacement, error) {
	if len(candidates) == 0 {
		return occupancyPlacement{}, fmt.Errorf("channel affinity shared placement has no candidates: key_fp=%s", keyFP)
	}
	if common.RedisEnabled && common.RDB != nil {
		cache := getChannelAffinityOccupancyCache()
		keys := make([]string, 0, len(candidates))
		for _, id := range candidates {
			keys = append(keys, cache.FullKey(strconv.Itoa(id)))
		}
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		res, err := occupancyPlaceSharedScript.Run(ctx, common.RDB, keys,
			keyFP, ttl.Milliseconds(), time.Now().UnixMilli(), occupancyLegacyMemberTTL().Milliseconds(), startIdx).Slice()
		if err != nil {
			return occupancyPlacement{}, fmt.Errorf("channel affinity shared placement failed: key_fp=%s err=%w", keyFP, err)
		}
		if len(res) != 2 {
			return occupancyPlacement{}, fmt.Errorf("channel affinity shared placement unexpected reply: key_fp=%s reply=%v", keyFP, res)
		}
		idx, _ := res[0].(int64)
		holderCount, _ := res[1].(int64)
		if idx < 0 || int(idx) >= len(candidates) {
			return occupancyPlacement{}, fmt.Errorf("channel affinity shared placement index out of range: key_fp=%s idx=%d", keyFP, idx)
		}
		return occupancyPlacement{ChannelID: candidates[idx], HolderCount: int(holderCount)}, nil
	}

	occupancyPlacementLock.Lock()
	defer occupancyPlacementLock.Unlock()

	cache := getChannelAffinityOccupancyCache()
	nowMs := time.Now().UnixMilli()
	counts := make([]int, len(candidates))
	minCount := -1
	for i, id := range candidates {
		entry, found, err := cache.Get(strconv.Itoa(id))
		if err != nil {
			return occupancyPlacement{}, fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", id, err)
		}
		n := 0
		if found {
			for _, fp := range occupancyEntryMembers(entry, nowMs) {
				if fp != keyFP {
					n++
				}
			}
		}
		counts[i] = n
		if minCount == -1 || n < minCount {
			minCount = n
		}
	}
	tier := make([]int, 0, len(candidates))
	for i, n := range counts {
		if n == minCount {
			tier = append(tier, i)
		}
	}
	pick := tier[startIdx%len(tier)]
	channelID := candidates[pick]
	if err := occupancyAddKeyFP(channelID, keyFP, ttl); err != nil {
		return occupancyPlacement{}, err
	}
	return occupancyPlacement{ChannelID: channelID, HolderCount: minCount}, nil
}

// occupancyPlacementLock 为内存模式满载落位的进程内全局串行锁：决策（重读实时
// 占用）与登记在锁内一次完成，避免并发键基于各自陈旧快照同时挤入同一渠道。
// Redis 模式由脚本原子性保证，锁退化为本地收敛。
var occupancyPlacementLock sync.Mutex

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
// "读条目 -> 登记 -> 写回"的原子执行；内存模式回退 occupancyAddKeyFP（渠道条目
// 分段锁保证）。满载降级路径已改经 occupancyPlaceSharedLeast 原子最小落位，
// 本函数保留给迁移登记（registerBindingIndexes）与直接登记语义的场景使用。
func occupancyAddKeyFPAtomic(channelID int, keyFP string, ttl time.Duration) error {
	if common.RedisEnabled && common.RDB != nil {
		cache := getChannelAffinityOccupancyCache()
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		fullKey := cache.FullKey(strconv.Itoa(channelID))
		return occupancyAddScript.Run(ctx, common.RDB, []string{fullKey},
			keyFP, ttl.Milliseconds(), time.Now().UnixMilli(), occupancyLegacyMemberTTL().Milliseconds()).Err()
	}
	return occupancyAddKeyFP(channelID, keyFP, ttl)
}

// occupancyClaimResult 为独占条件占位结果：claimed 为 true 表示本键已独占该渠道
// （条目此前为空或已含本键指纹）；false 表示条目已含其它键指纹（冲突），
// holderCount 为冲突时该渠道已绑定键数，供决策方重选。
type occupancyClaimResult struct {
	Claimed     bool
	HolderCount int
}

// occupancyClaimExclusive 独占条件占位（check-then-act 竞态的原子化，原 D3 根治）：
// 存活成员集为空或仅含本键指纹时登记成功（惰性剔除过期成员后判定）；已含其它存活
// 指纹时不动条目并返回冲突与键数。Redis 模式经 Lua 脚本原子执行（跨实例 CAS）；
// 内存模式在渠道条目分段锁内读-判-写，跨键并发对同一渠道串行。读或写失败返回
// error 由调用方降级（SSOT 5.1.5）。
func occupancyClaimExclusive(channelID int, keyFP string, ttl time.Duration) (occupancyClaimResult, error) {
	cache := getChannelAffinityOccupancyCache()
	key := strconv.Itoa(channelID)
	if common.RedisEnabled && common.RDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		res, err := occupancyClaimExclusiveScript.Run(ctx, common.RDB, []string{cache.FullKey(key)},
			keyFP, ttl.Milliseconds(), time.Now().UnixMilli(), occupancyLegacyMemberTTL().Milliseconds()).Int()
		if err != nil {
			return occupancyClaimResult{}, fmt.Errorf("channel affinity occupancy claim failed: channel=%d err=%w", channelID, err)
		}
		if res == 1 {
			return occupancyClaimResult{Claimed: true}, nil
		}
		return occupancyClaimResult{Claimed: false, HolderCount: -res}, nil
	}

	lock := occupancyEntryLock(channelID)
	lock.Lock()
	defer lock.Unlock()
	entry, found, err := cache.Get(key)
	if err != nil {
		return occupancyClaimResult{}, fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", channelID, err)
	}
	nowMs := time.Now().UnixMilli()
	next := map[string]int64{}
	if found {
		for _, fp := range occupancyEntryMembers(entry, nowMs) {
			next[fp] = entry.KeyFPs[fp]
		}
	}
	if _, mine := next[keyFP]; mine {
		// 本键指纹已在条目内（重绑/续期）：仅续期本成员，条目只延长不截断。
		next[keyFP] = nowMs + ttl.Milliseconds()
		entryTTL := ttl
		if remaining := occupancyRemainingTTL(cache, key); remaining > entryTTL {
			entryTTL = remaining
		}
		if err := cache.SetWithTTL(key, channelAffinityOccupancyEntry{KeyFPs: next}, entryTTL); err != nil {
			return occupancyClaimResult{}, fmt.Errorf("channel affinity occupancy renew failed: channel=%d err=%w", channelID, err)
		}
		occupancyMemExpireAt.Store(key, time.Now().Add(entryTTL))
		return occupancyClaimResult{Claimed: true}, nil
	}
	if len(next) > 0 {
		return occupancyClaimResult{Claimed: false, HolderCount: len(next)}, nil
	}
	next[keyFP] = nowMs + ttl.Milliseconds()
	if err := cache.SetWithTTL(key, channelAffinityOccupancyEntry{KeyFPs: next}, ttl); err != nil {
		return occupancyClaimResult{}, fmt.Errorf("channel affinity occupancy write failed: channel=%d err=%w", channelID, err)
	}
	occupancyMemExpireAt.Store(key, time.Now().Add(ttl))
	return occupancyClaimResult{Claimed: true}, nil
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
// （SSOT 4.2.4 规则1）：遍历全部渠道条目（惰性剔除过期成员），键数为 1 的渠道
// 贡献 1 记独占、键数大于 1 的渠道贡献其键数记复用，不区分规则来源（渠道全局
// 口径）。索引键遍历失败返回 (0, 0) 并记 SysError（03 文档 3.2 边界值：失败返回 0）。
func GetChannelAffinityExclusiveStats() (exclusive int, shared int) {
	cache := getChannelAffinityOccupancyCache()
	keys, err := cache.Keys()
	if err != nil {
		common.SysError(fmt.Sprintf("channel affinity occupancy list keys failed: err=%v", err))
		return 0, 0
	}
	nowMs := time.Now().UnixMilli()
	for _, k := range keys {
		entry, found, err := cache.Get(k)
		if err != nil || !found {
			continue
		}
		n := len(occupancyEntryMembers(entry, nowMs))
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

// occupancyEntryLocks 为占用索引条目分段锁：按渠道 ID 哈希取锁，内存模式下
// 串行化登记/移除的读-改-写。Redis 模式经 Lua 原子执行，锁退化为本地收敛，
// 语义不变。缺这层锁时跨键并发对同一渠道条目的 GET→改→SET 会 lost update，
// 成员丢失导致占用被低估、后续独占判定误判空闲（进程内等价于登记脚本要解决的
// 跨实例问题，见 occupancyAddScript 注释）。
var occupancyEntryLocks [64]sync.Mutex

func occupancyEntryLock(channelID int) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strconv.Itoa(channelID)))
	idx := h.Sum32() % uint32(len(occupancyEntryLocks))
	return &occupancyEntryLocks[idx]
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
// （正向绑定、占用索引、最近绑定记录）。独占占位经 occupancyClaimExclusive
// 条件占位（原 SSOT 8.3 D3 竞态根治）：候选渠道被并发键抢先占入时收到冲突，
// 从空闲集剔除后重选，循环直至占位成功或满载降级。索引读写任一失败时回滚本次
// 已写入的占位、置存储降级标记并降级软亲和返回 (0, false)（SSOT 5.1.5，降级请求
// 的随机选路结果不固化）；锁内 double-check 命中即读胜者结果使用同一渠道
// （SSOT 5.1.5）。
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
		markChannelAffinityStorageDegrade(c)
		return 0, false
	}

	// 构造 occupancyCounts：本键指纹已登记的渠道键数减 1（本键既有绑定视为空闲）。
	occupancyCounts := make(map[int]int, len(candidates))
	for _, id := range candidates {
		count, fps, err := occupancyBindingCount(id)
		if err != nil {
			common.SysError(fmt.Sprintf("channel affinity occupancy read failed: channel=%d key_fp=%s err=%v", id, meta.KeyFingerprint, err))
			markChannelAffinityStorageDegrade(c)
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

	// 条件占位循环（原 D3 竞态根治）：独占（exclusive）决策在快照上选定渠道，
	// 占位经 occupancyClaimExclusive 验证渠道此刻确实空闲；并发键抢先占入时收到
	// 冲突，剔除该渠道后重选。重选转入满载分支时不再使用陈旧快照直接登记：
	// 满载落位经 occupancyPlaceSharedLeast 在服务端原子重读实时占用、于最少绑定
	// 数层内选定并登记（并发键串行分摊，根治集中挤入同一渠道的堆积）。
	// 服务端复核发现空闲（HolderCount==0）时按独占终态处理，不计降级。
	decision := decideExclusiveBinding(candidates, occupancyCounts, lastBindRecord.ChannelID, lastBindFound, exclusivePickRandom)
	for decision.ChannelID > 0 {
		if decision.Mode == affinityBindModeShared {
			// 满载复用：原子最小落位（决策与登记服务端一次完成）。
			placement, err := occupancyPlaceSharedLeast(candidates, meta.KeyFingerprint, ttl, exclusivePickRandom(candidates)-candidates[0])
			if err != nil {
				common.SysError(fmt.Sprintf("channel affinity occupancy shared placement failed: key_fp=%s err=%v", meta.KeyFingerprint, err))
				markChannelAffinityStorageDegrade(c)
				return 0, false
			}
			decision.ChannelID = placement.ChannelID
			if placement.HolderCount == 0 {
				// 服务端复核仍有空闲渠道：快照误判满载（并发键已释放），按独占终态。
				decision.Mode = affinityBindModeExclusive
			}
			occupancyCounts[decision.ChannelID] = placement.HolderCount + 1
			break
		}
		claim, err := occupancyClaimExclusive(decision.ChannelID, meta.KeyFingerprint, ttl)
		if err != nil {
			common.SysError(fmt.Sprintf("channel affinity occupancy claim failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
			markChannelAffinityStorageDegrade(c)
			return 0, false
		}
		if claim.Claimed {
			break
		}
		// 冲突：决策依据的快照已失效，更新占用数后在剩余候选上重选。
		occupancyCounts[decision.ChannelID] = claim.HolderCount
		if occupancyCounts[decision.ChannelID] <= 0 {
			occupancyCounts[decision.ChannelID] = 1
		}
		decision = decideExclusiveBinding(candidates, occupancyCounts, lastBindRecord.ChannelID, lastBindFound, exclusivePickRandom)
	}
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
			markChannelAffinityStorageDegrade(c)
			// 占位已写入占用索引，正向写入失败需回滚索引，避免残留占坑。
			occupancyRemoveKeyFP(decision.ChannelID, meta.KeyFingerprint)
			return 0, false
		}
		if !ok {
			// 跨实例竞争失败：读胜者结果使用同一渠道（SSOT 5.1.5），与内存模式锁内
			// double-check 语义一致：跳过降级计数、不重复占位三写（占位三写由胜者完成）。
			winnerID, found, err := cache.Get(cacheKeySuffix)
			if err != nil || !found || winnerID <= 0 {
				common.SysError(fmt.Sprintf("channel affinity exclusive loser confirm failed: channel=%d key_fp=%s found=%v err=%v", decision.ChannelID, meta.KeyFingerprint, found, err))
				// 确认失败同样置存储降级标记：败者的正向写入已被拒绝，随机选路
				// 结果若被 RecordChannelAffinity 固化（old=0 追加登记），会绕过
				// 独占判定把随机渠道钉一个 TTL 周期（SSOT 5.1.5 降级语义）。
				markChannelAffinityStorageDegrade(c)
				// 本实例在败选渠道上的 claim 占位是幽灵占用：占用高估会使后续键
				// 误判满载进入 shared 降级，集中复用形成堆积，必须回滚。
				occupancyRemoveKeyFP(decision.ChannelID, meta.KeyFingerprint)
				return 0, false
			}
			// 同上：竞争失败记胜者渠道为首次占位渠道，供迁移移除与终态回滚使用。
			if c != nil {
				c.Set(ginKeyChannelAffinityBoundChannel, winnerID)
			}
			// 败者本实例在败选渠道（decision.ChannelID）上的 claim 占位与胜者绑定
			// 渠道（winnerID）分属两个渠道：败选渠道上的占位是幽灵占用，占用高估
			// 污染后续键的空闲判定（误判满载 -> shared 集中复用堆积），必须回滚。
			// 仅 decision != winner 时移除：胜者未跨渠道漂移时其占位三写是合法状态
			//（decision==winner 且 SetNX 失败只可能来自跨实例并发读旧值，此处读回
			// 相同值说明胜者绑定恰为本实例选定的渠道，占位继续有效）。
			if decision.ChannelID != winnerID {
				occupancyRemoveKeyFP(decision.ChannelID, meta.KeyFingerprint)
			}
			return winnerID, true
		}
	} else {
		// 内存模式：分段锁临界区内串行写入，进程内先到先得。
		if err := cache.SetWithTTL(cacheKeySuffix, decision.ChannelID, ttl); err != nil {
			common.SysError(fmt.Sprintf("channel affinity exclusive forward write failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
			markChannelAffinityStorageDegrade(c)
			occupancyRemoveKeyFP(decision.ChannelID, meta.KeyFingerprint)
			return 0, false
		}
	}

	// 胜者收尾：写最近绑定记录（正向绑定与占用索引已写入）。
	// 任一步失败即回滚正向绑定与占用占位后降级软亲和：残留孤儿正向绑定无占位
	// 三写配套（终态失败时无 boundChannel 标记可回滚），按独占未生效处理
	// （SSOT 5.1.5），清除失败记 SysError，残留随 TTL 过期。
	if err := lastBindSet(cacheKeySuffix, decision.ChannelID, 2*ttl); err != nil {
		common.SysError(fmt.Sprintf("channel affinity last bind write failed: channel=%d key_fp=%s err=%v", decision.ChannelID, meta.KeyFingerprint, err))
		markChannelAffinityStorageDegrade(c)
		rollbackBindingPlacement(cache.FullKey(cacheKeySuffix), meta.KeyFingerprint, decision.ChannelID)
		return 0, false
	}

	// 占位完成后读回正向缓存确认胜者结果。读回失败同样回滚：三处占位已全部写入，
	// 若放行降级软亲和，请求成功后 RecordChannelAffinity 会以 old=0 在另一渠道
	// 重复登记同一指纹，双渠道占位污染独占判定与统计（SSOT 5.1.5 任一步失败即回滚）。
	winnerID, found, err := cache.Get(cacheKeySuffix)
	if err != nil || !found || winnerID <= 0 {
		common.SysError(fmt.Sprintf("channel affinity exclusive winner confirm failed: channel=%d key_fp=%s found=%v err=%v", decision.ChannelID, meta.KeyFingerprint, found, err))
		markChannelAffinityStorageDegrade(c)
		rollbackBindingPlacement(cache.FullKey(cacheKeySuffix), meta.KeyFingerprint, decision.ChannelID)
		return 0, false
	}

	// 占位模式下独占判定已由条件占位闭环：Mode=exclusive 且渠道被并发键挤入的
	// 状态在 claim 冲突重试中已消除，进入 shared 判定仅剩满载降级一种来源。
	// 满载降级的 channel_binding_count 取原子落位返回的选中前其它键数
	//（occupancyPlaceSharedLeast 的服务端实时值，claim 循环快照仅作兜底）。
	bindingCount := occupancyCounts[winnerID] - 1
	if bindingCount < 0 {
		bindingCount = 0
	}
	if decision.Mode == affinityBindModeShared {
		// 满载降级：计数器原子加 1，gin context 写降级标记（04 文档 §4.3 结构，
		// 供 MarkChannelAffinityUsed 合并进 admin_info），同步输出与请求关联的
		// LogWarn（SSOT 5.3.2 第1、2条）。nil context 下仅计数与日志（logHelper
		// 的 ctx.Value 在 nil *gin.Context 上会 panic，与下方 c==nil 守卫同口径）。
		atomic.AddUint64(&channelAffinityDegradedReuseTotal, 1)
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

// occupancyHopScript 为独占命中续期时满载残留收敛的原子 hop：单次脚本执行内
// 复核迁移前提并完成迁移写，消除 check-then-act 窗口。
// KEYS[1] 旧渠道 occupancy 条目，KEYS[2..N] 候选渠道 occupancy 条目（与调用方
// candidates 顺序一致，含旧渠道），ARGV[1] 键指纹，ARGV[2] 绑定 TTL 毫秒，
// ARGV[3] 当前时刻毫秒，ARGV[4] v1 成员回退过期毫秒。
// 前提复核（服务端实时值）：本键指纹仍在旧渠道存活（并发 hop 已迁走时 no-op，
// 防止后到者以过期旧值再迁移造成指纹双渠道登记）、旧渠道存活成员（剔除本键）
// >0（本键仍在共享），且最少绑定数候选的键数 < 旧渠道其它键数（hop 后本键
// 所在渠道绑定数下降，分布的最大堆严格减小）。仅当目标为完全空闲（0 键）或
// 差距 >=2 时迁移：差距 1 时迁移只交换两渠道的绑定数（本键从 n+1 渠道到 n
// 渠道，最大堆不变），属无意义的迁移扰动。前提不成立时只读不写返回 {0,0}
//（绑定维持原值）；成立时在最少绑定层按 ARGV[5] 随机起点选定目标，从旧渠道
// 移除本键（空则 DEL，否则 KEEPTTL 写回），向目标登记本键（成员合并/过期
// 剔除/v1 重写/条目 PX 只延长不截断，与登记脚本同语义），返回
// {目标候选下标(0基), 旧渠道其它键数}。
// 注：脚本跨多键访问，要求 standalone Redis（项目 common.RDB 为单连接客户端）。
var occupancyHopScript = redis.NewScript(occupancyEntryPttlLua + `
local fp = ARGV[1]
local ttl_ms = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local legacy_ms = tonumber(ARGV[4])
local start_idx = tonumber(ARGV[5])

local function liveMembers(raw)
  local fps = {}
  if not raw or raw == '' then
    return fps
  end
  local ok, obj = pcall(cjson.decode, raw)
  if not ok or type(obj) ~= 'table' or type(obj.key_fps) ~= 'table' then
    return fps
  end
  local members = obj.key_fps
  if members[1] ~= nil then
    for _, m in ipairs(members) do
      fps[m] = 0
    end
  else
    for m, exp in pairs(members) do
      if exp == 0 or exp > now_ms then
        fps[m] = exp
      end
    end
  end
  return fps
end

local function rewriteV1(fps)
  for m, exp in pairs(fps) do
    if exp == 0 then
      fps[m] = now_ms + legacy_ms
    end
  end
end

local oldFps = liveMembers(redis.call('GET', KEYS[1]))
if oldFps[fp] == nil then
  -- 本键指纹已不在旧渠道（并发 hop 已迁移/回滚已清占位）：以旧值为据的本次
  -- hop 必须放弃，否则指纹被二次登记到另一渠道，双渠道幽灵占用污染空闲判定。
  return {0 - 1, 0}
end
local others = 0
for m, _ in pairs(oldFps) do
  if m ~= fp then
    others = others + 1
  end
end
if others == 0 then
  return {0 - 1, 0}
end

-- 候选实时其它键数，最少绑定层与其键数。
local counts = {}
local min_n = -1
for i = 2, #KEYS do
  local fps = liveMembers(redis.call('GET', KEYS[i]))
  local n = 0
  for m, _ in pairs(fps) do
    if m ~= fp then
      n = n + 1
    end
  end
  counts[i] = n
  if min_n < 0 or n < min_n then
    min_n = n
  end
end
-- 迁移有效性：目标层键数与旧渠道其它键数差距不足 2（且非完全空闲）时，
-- hop 无法减小分布的最大堆，维持原绑定（避免交换式扰动与迁移风暴）。
if min_n > 0 and others - min_n < 2 then
  return {0 - 1, others}
end
local tier = {}
for i = 2, #KEYS do
  if counts[i] == min_n then
    tier[#tier + 1] = i
  end
end
local pick = tier[(start_idx % #tier) + 1]

-- 旧渠道移除本键：剩余成员重写 v1 过期为 v2 后 KEEPTTL 写回（条目寿命不变），
-- 清空则 DEL。
oldFps[fp] = nil
local remain = 0
for _ in pairs(oldFps) do remain = remain + 1 end
if remain == 0 then
  redis.call('DEL', KEYS[1])
else
  rewriteV1(oldFps)
  redis.call('SET', KEYS[1], cjson.encode({key_fps = oldFps}), 'KEEPTTL')
end

-- 目标渠道登记本键：重读实时条目，v1 成员重写后并入本键，条目 PX 与登记
-- 脚本同口径只延长不截断。
local targetFps = liveMembers(redis.call('GET', KEYS[pick]))
rewriteV1(targetFps)
targetFps[fp] = now_ms + ttl_ms
redis.call('SET', KEYS[pick], cjson.encode({key_fps = targetFps}), 'PX', entryPttlMs(KEYS[pick], ttl_ms, now_ms))
return {pick - 1, others}
`)

// rebalanceExclusiveBinding 独占命中续期时的满载残留收敛：本键与其它键共享
// 渠道（满载降级或禁用扰动的遗产）而候选集已出现空闲渠道时，原子迁移到空闲
// 渠道并改写正向绑定与最近绑定记录。
// 残留来源（生产症状：渠道大量空闲时多键集中共享同一渠道）：渠道禁用把键挤到
// 少数渠道（合法满载降级），渠道恢复后存活键走亲和命中续期，绑定钉在原渠道，
// 堆积只能等整 TTL 到期重绑才消解（默认 3600s）。收敛提前到本键的下一请求：
// 命中续期发现共享 + 空闲即 hop，堆积在一个请求周期内自然排空。
// 决策与迁移写经 occupancyHopScript 服务端原子复核（快照过期时 no-op 维持原值），
// 内存模式在占位全局锁内重读实时占用后同语义执行。失败记 SysError 并维持原
// 绑定（收敛属尽力而为，残留随 TTL 收敛，与 SSOT 5.1.5 降级语义同口径）。
// 返回本请求应使用的渠道（hop 成功为新渠道，否则原渠道）。
func rebalanceExclusiveBinding(c *gin.Context, currentChannelID int, usingGroup string, modelName string) int {
	meta, ok := getChannelAffinityMeta(c)
	if !ok || meta.KeyFingerprint == "" {
		return currentChannelID
	}
	candidates := exclusiveCandidateChannels(c, meta, usingGroup, modelName)
	if len(candidates) == 0 {
		return currentChannelID
	}
	// 候选集不含当前绑定渠道（渠道被移出分组等）：亲和命中后的 distributor
	// 可用性复核会走清占位重绑，此处无须收敛。
	hasCurrent := false
	for _, id := range candidates {
		if id == currentChannelID {
			hasCurrent = true
			break
		}
	}
	if !hasCurrent {
		return currentChannelID
	}

	ttl := exclusiveAffinityTTL(meta)
	cacheKeySuffix := strings.TrimPrefix(meta.CacheKey, channelAffinityCacheNamespace+":")

	placement, err := occupancyHopLeast(candidates, currentChannelID, meta.KeyFingerprint, ttl, exclusivePickRandom(candidates)-candidates[0])
	if err != nil {
		common.SysError(fmt.Sprintf("channel affinity occupancy hop failed: channel=%d key_fp=%s err=%v", currentChannelID, meta.KeyFingerprint, err))
		return currentChannelID
	}
	if placement.ChannelID == 0 {
		return currentChannelID // 无共享或无空闲：绑定维持原值
	}

	// 正向绑定与最近绑定记录改写为 hop 结果（占用索引迁移已由脚本完成）。
	// 写失败时回滚占用索引恢复原渠道，维持原绑定（与 acquire 的回滚口径一致）。
	cache := getChannelAffinityCache()
	if err := cache.SetWithTTL(cacheKeySuffix, placement.ChannelID, ttl); err != nil {
		common.SysError(fmt.Sprintf("channel affinity hop forward write failed: channel=%d key_fp=%s err=%v", placement.ChannelID, meta.KeyFingerprint, err))
		occupancyRemoveKeyFP(placement.ChannelID, meta.KeyFingerprint)
		_ = occupancyAddKeyFP(currentChannelID, meta.KeyFingerprint, ttl)
		return currentChannelID
	}
	if err := lastBindSet(cacheKeySuffix, placement.ChannelID, 2*ttl); err != nil {
		common.SysError(fmt.Sprintf("channel affinity hop last bind write failed: channel=%d key_fp=%s err=%v", placement.ChannelID, meta.KeyFingerprint, err))
	}
	// 迁移后首占渠道更新为 hop 目标：终态失败回滚与后续迁移据此定位。
	if c != nil {
		c.Set(ginKeyChannelAffinityBoundChannel, placement.ChannelID)
	}
	common.SysLog(fmt.Sprintf(
		"channel affinity exclusive rebalance hop: rule=%s key_fp=%s from_channel=%d to_channel=%d freed_holders=%d",
		meta.RuleName, meta.KeyFingerprint, currentChannelID, placement.ChannelID, placement.HolderCount))
	return placement.ChannelID
}

// occupancyHopLeast 原子 hop 的存储层执行：Redis 模式经 occupancyHopScript
// 服务端原子完成复核与迁移；内存模式在占位全局锁内重读实时占用后同语义执行
//（进程内串行，等价语义）。返回 ChannelID=0 表示前提不成立（无共享或无空闲）。
func occupancyHopLeast(candidates []int, oldChannelID int, keyFP string, ttl time.Duration, startIdx int) (occupancyPlacement, error) {
	if common.RedisEnabled && common.RDB != nil {
		cache := getChannelAffinityOccupancyCache()
		keys := make([]string, 0, len(candidates)+1)
		keys = append(keys, cache.FullKey(strconv.Itoa(oldChannelID)))
		for _, id := range candidates {
			keys = append(keys, cache.FullKey(strconv.Itoa(id)))
		}
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		res, err := occupancyHopScript.Run(ctx, common.RDB, keys,
			keyFP, ttl.Milliseconds(), time.Now().UnixMilli(), occupancyLegacyMemberTTL().Milliseconds(), startIdx).Slice()
		if err != nil {
			return occupancyPlacement{}, fmt.Errorf("channel affinity hop script failed: key_fp=%s err=%w", keyFP, err)
		}
		if len(res) != 2 {
			return occupancyPlacement{}, fmt.Errorf("channel affinity hop unexpected reply: key_fp=%s reply=%v", keyFP, res)
		}
		idx, _ := res[0].(int64)
		holderCount, _ := res[1].(int64)
		if idx < 0 {
			return occupancyPlacement{ChannelID: 0, HolderCount: int(holderCount)}, nil
		}
		// 脚本 KEYS 布局为 [旧渠道, candidates[0], candidates[1], ...]，返回的
		// pick 是该数组 1 基下标：换算回 candidates 0 基下标须再减 1。
		candIdx := idx - 1
		if candIdx < 0 || int(candIdx) >= len(candidates) {
			return occupancyPlacement{}, fmt.Errorf("channel affinity hop index out of range: key_fp=%s idx=%d", keyFP, idx)
		}
		return occupancyPlacement{ChannelID: candidates[candIdx], HolderCount: int(holderCount)}, nil
	}

	occupancyPlacementLock.Lock()
	defer occupancyPlacementLock.Unlock()

	cache := getChannelAffinityOccupancyCache()
	nowMs := time.Now().UnixMilli()
	oldEntry, oldFound, err := cache.Get(strconv.Itoa(oldChannelID))
	if err != nil {
		return occupancyPlacement{}, fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", oldChannelID, err)
	}
	// 与脚本同口径：本键指纹已不在旧渠道（并发 hop 已迁移）时 no-op，
	// 防止后到者以过期旧值再迁移造成指纹双渠道登记。
	selfHeld := false
	others := 0
	if oldFound {
		for _, fp := range occupancyEntryMembers(oldEntry, nowMs) {
			if fp == keyFP {
				selfHeld = true
			} else {
				others++
			}
		}
	}
	if !selfHeld || others == 0 {
		return occupancyPlacement{ChannelID: 0}, nil
	}

	free := make([]int, 0, len(candidates))
	minCount := -1
	for _, id := range candidates {
		entry, found, err := cache.Get(strconv.Itoa(id))
		if err != nil {
			return occupancyPlacement{}, fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", id, err)
		}
		n := 0
		if found {
			for _, fp := range occupancyEntryMembers(entry, nowMs) {
				if fp != keyFP {
					n++
				}
			}
		}
		if minCount == -1 || n < minCount {
			minCount = n
		}
		if n == 0 {
			free = append(free, id)
		}
	}
	// 与脚本同口径的迁移有效性：目标层非空闲且差距不足 2 时维持原绑定。
	if minCount > 0 && others-minCount < 2 {
		return occupancyPlacement{ChannelID: 0, HolderCount: others}, nil
	}
	if len(free) == 0 {
		free = make([]int, 0, len(candidates))
		for _, id := range candidates {
			entry, found, err := cache.Get(strconv.Itoa(id))
			if err != nil {
				return occupancyPlacement{}, fmt.Errorf("channel affinity occupancy read failed: channel=%d err=%w", id, err)
			}
			n := 0
			if found {
				for _, fp := range occupancyEntryMembers(entry, nowMs) {
					if fp != keyFP {
						n++
					}
				}
			}
			if n == minCount {
				free = append(free, id)
			}
		}
	}
	pick := free[startIdx%len(free)]

	if err := occupancyRemoveKeyFP(oldChannelID, keyFP); err != nil {
		return occupancyPlacement{}, err
	}
	if err := occupancyAddKeyFP(pick, keyFP, ttl); err != nil {
		return occupancyPlacement{}, err
	}
	return occupancyPlacement{ChannelID: pick, HolderCount: others}, nil
}

func init() {
	operation_setting.OnRulesExclusiveBindChanged = HandleChannelAffinityRulesUpdate
}
