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
import { z } from 'zod'

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

/** Tri-state auto ban: 'unchanged' keeps each channel value (spec 4.2.2 section C). */
export type BatchEditAutoBan = 'unchanged' | 'enabled' | 'disabled'

/**
 * Count Unicode code points, matching the backend's utf8.RuneCountInString.
 * String.prototype.length counts UTF-16 code units, which counts astral
 * characters twice and would reject values the backend accepts.
 */
export const codePointLength = (value: string): number => [...value].length

/** Trims each comma-separated segment, mirroring the backend normalization. */
const normalizeCommaList = (value: string): string =>
  value
    .split(',')
    .map((segment) => segment.trim())
    .join(',')

const commaListSegments = (value: string): string[] =>
  value.split(',').map((segment) => segment.trim())

/** Trimmed value, blank means the field is skipped (spec 4.2.4 rule 1). */
const filled = (value: string | undefined): string => value?.trim() ?? ''

const optionalText = (maxLength: number, tooLong: string) =>
  z
    .string()
    .optional()
    .refine((value) => codePointLength(filled(value)) <= maxLength, tooLong)

const optionalInteger = (min: number, max: number, outOfRange: string) =>
  z.string().optional().refine((value) => {
    const raw = filled(value)
    if (!raw) return true
    const parsed = Number(raw)
    return Number.isSafeInteger(parsed) && parsed >= min && parsed <= max
  }, outOfRange)

/** Validate the mapping field for batch semantics; returns the i18n issue key or null. */
const modelMappingIssue = (value: string | undefined): string | null => {
  if (value === undefined) return null
  const trimmed = value.trim()
  // An explicitly emptied mapping field cannot mean "replace with empty": clearing
  // the mapping is only supported by the single channel editor.
  if (!trimmed) {
    return 'Model mapping cannot be empty here. Clear it in single-channel edit instead.'
  }
  // Reuse the single-channel validation: values must be strings, matching the
  // backend's map[string]string unmarshal on the relay path.
  const { valid, error } = validateModelMappingJson(trimmed)
  if (valid) return null
  return error ?? 'Model mapping must be a valid JSON object'
}

/**
 * Drawer form schema: every field is optional, blank means skip (spec 4.2.4 rule 1).
 * Issue messages are i18n keys, rendered through t() by FormMessage.
 * The selected ids are validated separately by validateBatchEditTargets: they are not
 * form state, so a selection change never resets what the user has typed.
 */
export const channelBatchEditSchema = z.object({
  group: z
    .string()
    .optional()
    .refine(
      (value) => codePointLength(filled(value)) <= MAX_GROUP_LENGTH,
      'Group must be less than 64 characters'
    )
    .refine(
      (value) =>
        !filled(value) ||
        !commaListSegments(filled(value)).some((segment) => segment === ''),
      'Group format error'
    ),
  tag: optionalText(MAX_TAG_LENGTH, 'Tag must be less than 191 characters'),
  remark: optionalText(
    MAX_TEXT_LENGTH,
    'Remark must be less than 255 characters'
  ),
  models: z
    .string()
    .optional()
    .refine(
      (value) =>
        !filled(value) ||
        !commaListSegments(filled(value)).some((segment) => segment === ''),
      'Model list format error'
    )
    .refine(
      (value) =>
        commaListSegments(filled(value)).every(
          (name) => codePointLength(name) <= MAX_TEXT_LENGTH
        ),
      'Model name must be less than 255 characters'
    ),
  model_mapping: z
    .string()
    .optional()
    .refine((value) => modelMappingIssue(value) === null, {
      error: (issue) =>
        modelMappingIssue(issue.input as string | undefined) ?? '',
    }),
  weight: optionalInteger(
    0,
    MAX_WEIGHT,
    'Weight must be between 0 and 4294967295'
  ),
  priority: optionalInteger(
    -MAX_SAFE_PRIORITY,
    MAX_SAFE_PRIORITY,
    'Priority is out of range'
  ),
  test_model: optionalText(
    MAX_TEXT_LENGTH,
    'Test model must be less than 255 characters'
  ),
  auto_ban: z.enum(['unchanged', 'enabled', 'disabled']).optional(),
})

export type ChannelBatchEditFormValues = z.infer<typeof channelBatchEditSchema>

/** An effective field of a built payload: payload key plus display label. */
export interface BatchEditField {
  key: keyof BatchUpdateParams
  label: string
}

/** One editable column: payload key, display label and form value reader. */
interface BatchEditFieldDescriptor {
  key: keyof BatchUpdateParams
  label: string
  /** Effective payload value, undefined when the field is blank (skipped). */
  read: (values: ChannelBatchEditFormValues) => string | number | undefined
}

/**
 * Single source of the effective field list: the confirmation dialog, the payload
 * and the field summaries all derive from it, so they cannot drift apart.
 */
const BATCH_EDIT_FIELDS: BatchEditFieldDescriptor[] = [
  {
    key: 'group',
    label: 'Group',
    read: (values) => {
      const group = filled(values.group)
      return group ? normalizeCommaList(group) : undefined
    },
  },
  {
    key: 'tag',
    label: 'Tag',
    read: (values) => filled(values.tag) || undefined,
  },
  {
    key: 'remark',
    label: 'Remark',
    read: (values) => filled(values.remark) || undefined,
  },
  {
    key: 'models',
    label: 'Models',
    read: (values) => {
      const models = filled(values.models)
      return models ? normalizeCommaList(models) : undefined
    },
  },
  {
    key: 'model_mapping',
    label: 'Model Mapping',
    read: (values) => filled(values.model_mapping) || undefined,
  },
  {
    key: 'weight',
    label: 'Weight',
    read: (values) => {
      const raw = filled(values.weight)
      return raw ? Number(raw) : undefined
    },
  },
  {
    key: 'priority',
    label: 'Priority',
    read: (values) => {
      const raw = filled(values.priority)
      return raw ? Number(raw) : undefined
    },
  },
  {
    key: 'test_model',
    label: 'Test Model',
    read: (values) => filled(values.test_model) || undefined,
  },
  {
    key: 'auto_ban',
    label: 'Auto Ban',
    read: (values) => {
      if (values.auto_ban === 'enabled') return 1
      if (values.auto_ban === 'disabled') return 0
      return undefined
    },
  },
]

/**
 * Validate the selection boundary (spec 4.2.2 / 4.2.4 rule 3): returns the i18n
 * issue key when the selection cannot be submitted, otherwise null.
 */
export function validateBatchEditTargets(ids: number[]): string | null {
  if (ids.length === 0) return 'No channels selected'
  if (ids.length > MAX_BATCH_EDIT_CHANNELS) {
    return `The number of selected channels exceeds the limit of ${MAX_BATCH_EDIT_CHANNELS}. Please split the operation.`
  }
  return null
}

export interface BatchEditSubmission {
  /** Submittable payload, null when no field is effective */
  payload: BatchUpdateParams | null
  /** Effective fields (payload key + English label) for the confirm dialog */
  fields: BatchEditField[]
}

/** Build the batch edit payload from validated form values (spec 4.2.4 rules 1/6). */
export function buildBatchEditSubmission(
  ids: number[],
  values: ChannelBatchEditFormValues
): BatchEditSubmission {
  const payload: BatchUpdateParams = { ids }
  const fields: BatchEditField[] = []

  for (const descriptor of BATCH_EDIT_FIELDS) {
    const value = descriptor.read(values)
    if (value === undefined) continue
    ;(
      payload as unknown as Record<string, string | number | number[]>
    )[descriptor.key] = value
    fields.push({ key: descriptor.key, label: descriptor.label })
  }

  if (fields.length === 0) {
    return { payload: null, fields: [] }
  }
  return { payload, fields }
}
