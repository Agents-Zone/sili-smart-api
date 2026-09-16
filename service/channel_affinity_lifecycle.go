package service

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/go-redis/redis/v8"
)

// 三套绑定数据在同一次 Redis 操作中更新，旧请求只能操作仍指向旧渠道的绑定。
// KEYS 为占用索引、正向绑定、最近绑定；前四个参数沿用占用索引脚本。
var releaseDisabledAffinityScript = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[4] then return 0 end
redis.call('DEL', KEYS[2], KEYS[3])
` + occupancyRemoveLua)

var renewExclusiveAffinityScript = redis.NewScript(occupancyEntryPttlLua + `
if redis.call('GET', KEYS[2]) ~= ARGV[5] then return 0 end
redis.call('PEXPIRE', KEYS[2], ARGV[2])
redis.call('SET', KEYS[3], ARGV[6], 'PX', tonumber(ARGV[2]) * 2)
` + occupancyAddLua)

// releaseDisabledAffinityBinding 在确认手动或自动禁用后释放旧绑定。
// 返回 true 表示需重新获取绑定，即使并发请求已经完成了释放或重绑。
func releaseDisabledAffinityBinding(meta channelAffinityMeta, channelID int) (bool, error) {
	channel, err := model.CacheGetChannel(channelID)
	if err != nil {
		return false, err
	}
	if channel == nil || (channel.Status != common.ChannelStatusManuallyDisabled && channel.Status != common.ChannelStatusAutoDisabled) {
		return false, nil
	}
	suffix := strings.TrimPrefix(meta.CacheKey, channelAffinityCacheNamespace+":")
	lock := channelAffinityBindLock(suffix)
	lock.Lock()
	defer lock.Unlock()
	cache := getChannelAffinityCache()
	if common.RedisEnabled && common.RDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		err := releaseDisabledAffinityScript.Run(ctx, common.RDB, []string{
			getChannelAffinityOccupancyCache().FullKey(strconv.Itoa(channelID)),
			cache.FullKey(suffix), getChannelAffinityLastBindCache().FullKey(suffix),
		}, meta.KeyFingerprint, time.Now().UnixMilli(), occupancyFallbackTTL().Milliseconds(), strconv.Itoa(channelID)).Err()
		return true, err
	}
	current, found, err := cache.Get(suffix)
	if err != nil || !found || current != channelID {
		return true, err
	}
	if _, err := cache.DeleteMany([]string{suffix}); err != nil {
		return true, err
	}
	if err := occupancyRemoveKeyFP(channelID, meta.KeyFingerprint); err != nil {
		return true, err
	}
	return true, lastBindDelete(suffix)
}

// renewExclusiveAffinityBinding 只续期仍存在且渠道一致的绑定，防止旧请求复活旧绑定。
func renewExclusiveAffinityBinding(meta channelAffinityMeta, channelID int, ttl time.Duration) error {
	suffix := strings.TrimPrefix(meta.CacheKey, channelAffinityCacheNamespace+":")
	lock := channelAffinityBindLock(suffix)
	lock.Lock()
	defer lock.Unlock()
	channel, err := model.CacheGetChannel(channelID)
	if err != nil {
		return err
	}
	if channel == nil || channel.Status != common.ChannelStatusEnabled {
		return nil
	}
	cache := getChannelAffinityCache()
	if common.RedisEnabled && common.RDB != nil {
		record, err := common.Marshal(channelAffinityLastBindRecord{ChannelID: channelID, BoundAt: time.Now().Unix()})
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		return renewExclusiveAffinityScript.Run(ctx, common.RDB, []string{
			getChannelAffinityOccupancyCache().FullKey(strconv.Itoa(channelID)),
			cache.FullKey(suffix), getChannelAffinityLastBindCache().FullKey(suffix),
		}, meta.KeyFingerprint, ttl.Milliseconds(), time.Now().UnixMilli(), occupancyLegacyMemberTTL().Milliseconds(), strconv.Itoa(channelID), string(record)).Err()
	}
	current, found, err := cache.Get(suffix)
	if err != nil || !found || current != channelID {
		return err
	}
	if err := cache.SetWithTTL(suffix, channelID, ttl); err != nil {
		return err
	}
	if err := occupancyAddKeyFP(channelID, meta.KeyFingerprint, ttl); err != nil {
		return err
	}
	return lastBindSet(suffix, channelID, 2*ttl)
}
