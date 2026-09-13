/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import type { BatchUpdateParams } from '../types'
import { validateModelMappingJson } from './model-mapping-validation'

/** Single batch edit upper bound (spec 4.2.4 rule 3). */
export const MAX_BATCH_EDIT_CHANNELS = 200

/** Weight upper bound, aligned with the channels.weight column (uint32). */
const MAX_WEIGHT = 4294967295

/** Priority bounds, aligned with Number.isSafeInteger (±(2^53-1)). */
const MAX_SAFE_PRIORITY = 9007199254740991

/** Group length limit (spec 4.2.2 section A). */
const MAX_GROUP_LENGTH = 64

/** Tag length limit, aligned with the channels.tag column width. */
const MAX_TAG_LENGTH = 191

/** Remark / test model / single model name length limit. */
const MAX_TEXT_LENGTH = 255

/** Form model of the batch edit drawer: every field is optional, auto_ban is tri-state. */
export interface ChannelBatchEditForm {
  group?: string
  tag?: string
  remark?: string
  models?: string
  model_mapping?: string
  weight?: string
  priority?: string
  test_model?: string
  auto_ban?: 'unchanged' | 'enabled' | 'disabled'
}

/** An effective field of a built payload: payload key plus display label. */
export interface BatchEditField {
  key: keyof BatchUpdateParams
  label: string
}

export interface BatchEditBuildResult {
  /** Submittable payload when validation passes, otherwise null */
  payload: BatchUpdateParams | null
  /** Effective fields (payload key + English label) for the confirm dialog */
  fields: BatchEditField[]
  /** Validation failure reason (i18n key), null on success */
  error: string | null
}

const emptyResult = (error: string): BatchEditBuildResult => ({
  payload: null,
  fields: [],
  error,
})

/** Trims each comma-separated segment, mirroring the backend normalization. */
const normalizeCommaList = (value: string): string =>
  value
    .split(',')
    .map((segment) => segment.trim())
    .join(',')

/**
 * Validates the form and builds the batch edit payload:
 * ids boundary -> per-field validation -> effective field count.
 * Fields left blank (undefined or empty after trim) stay out of the payload.
 */
export function buildBatchEditPayload(
  ids: number[],
  form: ChannelBatchEditForm
): BatchEditBuildResult {
  if (ids.length === 0) {
    return emptyResult('No channels selected')
  }

  if (ids.length > MAX_BATCH_EDIT_CHANNELS) {
    return emptyResult(
      `The number of selected channels exceeds the limit of ${MAX_BATCH_EDIT_CHANNELS}. Please split the operation.`
    )
  }

  const group = form.group?.trim() ?? ''
  const tag = form.tag?.trim() ?? ''
  const remark = form.remark?.trim() ?? ''
  const models = form.models?.trim() ?? ''
  const testModel = form.test_model?.trim() ?? ''
  const rawWeight = form.weight?.trim() ?? ''
  const rawPriority = form.priority?.trim() ?? ''

  if (group) {
    if (group.length > MAX_GROUP_LENGTH) {
      return emptyResult('Group must be less than 64 characters')
    }
    if (group.split(',').some((segment) => segment.trim() === '')) {
      return emptyResult('Group format error')
    }
  }

  if (tag.length > MAX_TAG_LENGTH) {
    return emptyResult('Tag must be less than 191 characters')
  }

  if (remark.length > MAX_TEXT_LENGTH) {
    return emptyResult('Remark must be less than 255 characters')
  }

  if (testModel.length > MAX_TEXT_LENGTH) {
    return emptyResult('Test model must be less than 255 characters')
  }

  if (models) {
    const modelNames = models.split(',').map((segment) => segment.trim())
    if (modelNames.some((name) => name === '')) {
      return emptyResult('Model list format error')
    }
    if (modelNames.some((name) => name.length > MAX_TEXT_LENGTH)) {
      return emptyResult('Model name must be less than 255 characters')
    }
  }

  // An explicitly emptied mapping field cannot mean "replace with empty": clearing
  // the mapping is only supported by the single channel editor.
  const modelMapping = form.model_mapping?.trim()
  if (modelMapping === '') {
    return emptyResult(
      'Model mapping cannot be empty here. Clear it in single-channel edit instead.'
    )
  }
  if (modelMapping) {
    // Reuse the single-channel validation: values must be strings, matching the
    // backend's map[string]string unmarshal on the relay path.
    const { valid } = validateModelMappingJson(modelMapping)
    if (!valid) {
      return emptyResult('Model mapping must be a valid JSON object')
    }
  }

  let weight: number | undefined
  if (rawWeight) {
    const parsed = Number(rawWeight)
    if (!Number.isSafeInteger(parsed) || parsed < 0 || parsed > MAX_WEIGHT) {
      return emptyResult('Weight must be between 0 and 4294967295')
    }
    weight = parsed
  }

  let priority: number | undefined
  if (rawPriority) {
    const parsed = Number(rawPriority)
    if (
      !Number.isSafeInteger(parsed) ||
      Math.abs(parsed) > MAX_SAFE_PRIORITY
    ) {
      return emptyResult('Priority is out of range')
    }
    priority = parsed
  }

  let autoBan: number | undefined
  if (form.auto_ban === 'enabled') {
    autoBan = 1
  } else if (form.auto_ban === 'disabled') {
    autoBan = 0
  }

  const payload: BatchUpdateParams = { ids }
  const fields: BatchEditField[] = []

  // Single source of effective fields: each entry appends both the payload value
  // and its {key, label} pair, so the confirm dialog needs no reverse lookup.
  const appendField = (
    key: keyof BatchUpdateParams,
    label: string,
    value: string | number
  ) => {
    ;(payload as unknown as Record<string, string | number | number[]>)[key] =
      value
    fields.push({ key, label })
  }

  if (group) appendField('group', 'Group', normalizeCommaList(group))
  if (tag) appendField('tag', 'Tag', tag)
  if (remark) appendField('remark', 'Remark', remark)
  if (models) appendField('models', 'Models', normalizeCommaList(models))
  if (modelMapping) {
    appendField('model_mapping', 'Model Mapping', modelMapping)
  }
  if (weight !== undefined) appendField('weight', 'Weight', weight)
  if (priority !== undefined) appendField('priority', 'Priority', priority)
  if (testModel) appendField('test_model', 'Test Model', testModel)
  if (autoBan !== undefined) appendField('auto_ban', 'Auto Ban', autoBan)

  if (fields.length === 0) {
    return emptyResult('You must fill in at least one field')
  }

  return { payload, fields, error: null }
}
