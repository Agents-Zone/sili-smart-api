package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

var renewExclusiveAffinityScript = redis.NewScript(occupancyEntryPttlLua + `
if redis.call('GET', KEYS[2]) ~= ARGV[5] then return 0 end
redis.call('PEXPIRE', KEYS[2], ARGV[2])
redis.call('SET', KEYS[3], ARGV[6], 'PX', ARGV[7])
` + occupancyAddLua)

// 判断渠道归属与三套数据的清理必须在同一次脚本内完成，避免扫描后改绑被误删。
var clearChannelAffinityBindingScript = redis.NewScript(`
local current = redis.call('GET', KEYS[2])
if current and current ~= ARGV[4] then return 0 end
local history = redis.call('GET', KEYS[3])
if history then
  local ok, record = pcall(cjson.decode, history)
  if ok and type(record) == 'table' and tostring(record.channel_id) == ARGV[4] then
    redis.call('DEL', KEYS[3])
  end
end
local removed = redis.call('DEL', KEYS[2])
local function removeOccupancy()
` + occupancyRemoveLua + `
end
removeOccupancy()
return removed
`)

// clearChannelAffinityBinding 只清理仍属于指定渠道的绑定。keyFP 为空时从键后缀定位占用成员。
func clearChannelAffinityBinding(suffix string, channelID int, keyFP string) (bool, error) {
	lock := channelAffinityBindLock(suffix)
	lock.Lock()
	defer lock.Unlock()
	cache := getChannelAffinityCache()
	if keyFP == "" {
		_, members, err := occupancyBindingCount(channelID)
		if err != nil {
			return false, err
		}
		candidates := append(exclusiveKeyFingerprintsFromSuffix(suffix), affinityFingerprint(suffix))
		for _, candidate := range candidates {
			for _, member := range members {
				if candidate == member {
					keyFP = candidate
					break
				}
			}
			if keyFP != "" {
				break
			}
		}
	}
	if common.RedisEnabled && common.RDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveRedisOpTimeout)
		defer cancel()
		removed, err := clearChannelAffinityBindingScript.Run(ctx, common.RDB, []string{
			getChannelAffinityOccupancyCache().FullKey(strconv.Itoa(channelID)), cache.FullKey(suffix),
			getChannelAffinityLastBindCache().FullKey(suffix),
		}, keyFP, time.Now().UnixMilli(), occupancyFallbackTTL().Milliseconds(), strconv.Itoa(channelID)).Int()
		return removed > 0, err
	}
	current, found, err := cache.Get(suffix)
	if err != nil || (found && current != channelID) {
		return false, err
	}
	record, historyFound, err := lastBindGet(suffix)
	if err != nil {
		return false, err
	}
	if historyFound && record.ChannelID == channelID {
		if err := lastBindDelete(suffix); err != nil {
			return false, err
		}
	}
	if _, err := cache.DeleteMany([]string{suffix}); err != nil {
		return false, err
	}
	if keyFP != "" {
		if err := occupancyRemoveKeyFP(channelID, keyFP); err != nil {
			return found, err
		}
	}
	return found, nil
}

// releaseDisabledAffinityBinding 在确认手动禁用、自动禁用或删除后释放旧绑定。
// 返回 true 表示需重新获取绑定，即使并发请求已经完成了释放或重绑。
func releaseDisabledAffinityBinding(meta channelAffinityMeta, channelID int) (bool, error) {
	channel, err := model.CacheGetChannel(channelID)
	if err != nil || channel == nil {
		// 缓存缺失须由数据库确认；连接故障等错误不能当作渠道已删除。
		channel, err = model.GetChannelById(channelID, false)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return false, err
		}
	}
	if channel != nil && err == nil && channel.Status != common.ChannelStatusManuallyDisabled && channel.Status != common.ChannelStatusAutoDisabled {
		return false, nil
	}
	suffix := strings.TrimPrefix(meta.CacheKey, channelAffinityCacheNamespace+":")
	_, err = clearChannelAffinityBinding(suffix, channelID, meta.KeyFingerprint)
	return true, err
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
		}, meta.KeyFingerprint, ttl.Milliseconds(), time.Now().UnixMilli(), occupancyLegacyMemberTTL().Milliseconds(), strconv.Itoa(channelID), string(record), channelAffinityLastBindTTL(ttl).Milliseconds()).Err()
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
	return lastBindSet(suffix, channelID, channelAffinityLastBindTTL(ttl))
}
