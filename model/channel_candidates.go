package model

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// GetEnabledChannelIDsForGroupModel 返回分组+模型下全部可用渠道 ID（含全部优先级层级，
// 非单层），供渠道独占绑定的候选集使用（SSOT 5.1.3「可用渠道候选」）。内存缓存启用时读
// group2model2channels（含 normalized model 名回退）并复用 filterChannelsByRequestPathAndModel
// 的路径过滤语义；未启用时走 DB 查询（enabled=true 按 group+model 去重 channel_id，
// 复用 filterAbilitiesByRequestPathAndModel）。无候选时返回空切片。
func GetEnabledChannelIDsForGroupModel(group string, modelName string, requestPath string) []int {
	if group == "" || modelName == "" {
		return []int{}
	}

	if !common.MemoryCacheEnabled {
		return getEnabledChannelIDsFromDB(group, modelName, requestPath)
	}

	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	channels := filterChannelsByRequestPathAndModel(group2model2channels[group][modelName], requestPath, modelName)
	if len(channels) == 0 {
		normalizedModel := ratio_setting.FormatMatchingModelName(modelName)
		channels = filterChannelsByRequestPathAndModel(group2model2channels[group][normalizedModel], requestPath, modelName)
	}
	if len(channels) == 0 {
		return []int{}
	}
	ids := make([]int, len(channels))
	copy(ids, channels)
	return ids
}

// getEnabledChannelIDsFromDB 内存缓存未启用时的候选集查询：Ability 表按 group+model+enabled
// 过滤，去重 channel_id，复用现有路径过滤语义（仅 Advanced Custom 渠道路径校验，其余直通）。
func getEnabledChannelIDsFromDB(group string, modelName string, requestPath string) []int {
	var abilities []Ability
	if err := DB.Where(commonGroupCol+" = ? and model = ? and enabled = ?", group, modelName, true).Find(&abilities).Error; err != nil {
		return []int{}
	}
	abilities = filterAbilitiesByRequestPathAndModel(abilities, requestPath, modelName)

	seen := make(map[int]struct{}, len(abilities))
	ids := make([]int, 0, len(abilities))
	for _, ability := range abilities {
		if _, ok := seen[ability.ChannelId]; ok {
			continue
		}
		seen[ability.ChannelId] = struct{}{}
		ids = append(ids, ability.ChannelId)
	}
	return ids
}
