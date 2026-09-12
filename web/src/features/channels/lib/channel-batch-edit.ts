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

/** Single batch edit upper bound (spec 4.2.4 rule 3). */
export const MAX_BATCH_EDIT_CHANNELS = 200

/** Weight upper bound, aligned with the channels.weight column (uint32). */
const MAX_WEIGHT = 4294967295

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

export interface BatchEditBuildResult {
  /** Submittable payload when validation passes, otherwise null */
  payload: BatchUpdateParams | null
  /** Display names of the effective fields (English source text) for the confirm dialog and audit */
  fieldLabels: string[]
  /** Validation failure reason (i18n key), null on success */
  error: string | null
}

const emptyResult = (error: string): BatchEditBuildResult => ({
  payload: null,
  fieldLabels: [],
  error,
})

/** Splits a comma separated list into trimmed segments. */
const splitCommaList = (value: string): string[] =>
  value.split(',').map((segment) => segment.trim())

/** True when the text parses into a plain JSON object (arrays, null and scalars are not). */
const isJsonObject = (value: string): boolean => {
  try {
    const parsed = JSON.parse(value)
    return typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)
  } catch {
    return false
  }
}

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
    if (splitCommaList(group).some((segment) => segment === '')) {
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
    const modelNames = splitCommaList(models)
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
  if (modelMapping && !isJsonObject(modelMapping)) {
    return emptyResult('Model mapping must be a valid JSON object')
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
    if (!Number.isSafeInteger(parsed)) {
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
  const fieldLabels: string[] = []

  if (group) {
    payload.group = group
    fieldLabels.push('Group')
  }
  if (tag) {
    payload.tag = tag
    fieldLabels.push('Tag')
  }
  if (remark) {
    payload.remark = remark
    fieldLabels.push('Remark')
  }
  if (models) {
    payload.models = models
    fieldLabels.push('Models')
  }
  if (modelMapping) {
    payload.model_mapping = modelMapping
    fieldLabels.push('Model Mapping')
  }
  if (weight !== undefined) {
    payload.weight = weight
    fieldLabels.push('Weight')
  }
  if (priority !== undefined) {
    payload.priority = priority
    fieldLabels.push('Priority')
  }
  if (testModel) {
    payload.test_model = testModel
    fieldLabels.push('Test Model')
  }
  if (autoBan !== undefined) {
    payload.auto_ban = autoBan
    fieldLabels.push('Auto Ban')
  }

  if (fieldLabels.length === 0) {
    return emptyResult('You must fill in at least one field')
  }

  return { payload, fieldLabels, error: null }
}
