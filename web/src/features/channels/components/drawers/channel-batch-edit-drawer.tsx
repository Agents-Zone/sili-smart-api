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
import { useQuery, useQueryClient } from '@tanstack/react-query'
import type { TFunction } from 'i18next'
import { Loader2 } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import {
  sideDrawerContentClassName,
  sideDrawerFooterClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
  SideDrawerSection,
  SideDrawerSectionHeader,
} from '@/components/drawer-layout'
import { MultiSelect } from '@/components/multi-select'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Textarea } from '@/components/ui/textarea'

import { getAllModels, getGroups } from '../../api'
import {
  buildBatchEditPayload,
  handleBatchUpdate,
  type BatchUpdateOutcome,
  type ChannelBatchEditForm,
} from '../../lib'
import type { BatchUpdateFailure, BatchUpdateParams } from '../../types'

type ChannelBatchEditDrawerProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  selectedIds: number[]
  onCommitted?: () => void
  /** Submit implementation, defaults to handleBatchUpdate; tests inject a stub. */
  submit?: (payload: BatchUpdateParams) => Promise<BatchUpdateOutcome>
}

/** Validated payload of the last save click plus its effective field labels (spec 4.3.2). */
type BatchEditConfirmRequest = {
  payload: BatchUpdateParams
  fieldLabels: string[]
}

/** Overlong confirmation summaries are cut here and suffixed with an ellipsis (spec 4.3.5). */
const MAX_SUMMARY_LENGTH = 50

/** Display label -> payload key, mirroring the label list built by buildBatchEditPayload. */
const FIELD_PAYLOAD_KEYS: Record<string, keyof BatchUpdateParams> = {
  Group: 'group',
  Tag: 'tag',
  Remark: 'remark',
  Models: 'models',
  'Model Mapping': 'model_mapping',
  Weight: 'weight',
  Priority: 'priority',
  'Test Model': 'test_model',
  'Auto Ban': 'auto_ban',
}

/** Summary of one effective field: the value written into the payload (auto_ban is tri-state). */
function formatFieldSummary(
  label: string,
  payload: BatchUpdateParams,
  t: TFunction
): string {
  const value = payload[FIELD_PAYLOAD_KEYS[label]]
  if (value === undefined) return ''
  if (label === 'Auto Ban') return t(value === 1 ? 'Enabled' : 'Disabled')
  return String(value)
}

/** Truncated summary; the untruncated value stays available through the element title. */
const truncateSummary = (summary: string): string =>
  summary.length > MAX_SUMMARY_LENGTH
    ? `${summary.slice(0, MAX_SUMMARY_LENGTH)}…`
    : summary

/** Comma separated form value -> chip values. */
const toChipValues = (value?: string) =>
  value
    ? value
        .split(',')
        .map((item) => item.trim())
        .filter(Boolean)
    : []

export function ChannelBatchEditDrawer(props: ChannelBatchEditDrawerProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [form, setForm] = useState<ChannelBatchEditForm>({})
  const [pending, setPending] = useState<BatchEditConfirmRequest | null>(null)
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [failed, setFailed] = useState<BatchUpdateFailure[]>([])

  const { data: groupsData } = useQuery({
    queryKey: ['groups'],
    queryFn: getGroups,
  })

  const { data: allModelsData } = useQuery({
    queryKey: ['channel_models'],
    queryFn: getAllModels,
  })

  useEffect(() => {
    if (!props.open) return
    setForm({})
    setPending(null)
    setConfirmOpen(false)
    setIsSubmitting(false)
    setFailed([])
  }, [props.open])

  const groupOptions = useMemo(
    () =>
      (groupsData?.data ?? []).map((group) => ({
        value: group,
        label: group,
      })),
    [groupsData]
  )

  const modelOptions = useMemo(
    () =>
      (allModelsData?.data ?? []).map((model) => ({
        value: model.id,
        label: model.id,
      })),
    [allModelsData]
  )

  const autoBanOptions = [
    { value: 'unchanged', label: t('Keep unchanged') },
    { value: 'enabled', label: t('Enabled') },
    { value: 'disabled', label: t('Disabled') },
  ]

  const updateForm = (patch: Partial<ChannelBatchEditForm>) => {
    setForm((current) => ({ ...current, ...patch }))
  }

  const confirmFields = (pending?.fieldLabels ?? []).map((label) => ({
    label,
    summary: pending ? formatFieldSummary(label, pending.payload, t) : '',
  }))

  const defaultSubmit = (payload: BatchUpdateParams) =>
    handleBatchUpdate(payload, queryClient, props.onCommitted)

  const handleSave = () => {
    if (isSubmitting) return

    const result = buildBatchEditPayload(props.selectedIds, form)

    if (result.error) {
      // No effective field (or invalid input): warn instead of opening the dialog.
      toast.warning(t(result.error))
      return
    }

    if (!result.payload) return

    setFailed([])
    setPending({ payload: result.payload, fieldLabels: result.fieldLabels })
    setConfirmOpen(true)
  }

  const handleConfirm = async () => {
    if (!pending || isSubmitting) return

    const submit = props.submit ?? defaultSubmit
    setConfirmOpen(false)
    setIsSubmitting(true)
    setFailed([])

    const outcome = await submit(pending.payload)

    setIsSubmitting(false)
    if (outcome.ok) {
      // Success closes the drawer; the upstream selection is cleared via onCommitted.
      props.onOpenChange(false)
      return
    }

    // Failure keeps the drawer and the filled values so the user can retry.
    setFailed(outcome.failed)
  }

  return (
    <>
      <Sheet open={props.open} onOpenChange={props.onOpenChange}>
        <SheetContent className={sideDrawerContentClassName('sm:max-w-3xl')}>
          <SheetHeader className={sideDrawerHeaderClassName()}>
            <SheetTitle>{t('Channel Batch Edit')}</SheetTitle>
            <SheetDescription>
              {t('{{count}} channel(s) selected', {
                count: props.selectedIds.length,
              })}
            </SheetDescription>
          </SheetHeader>

          <div className={sideDrawerFormClassName()}>
            <Alert>
              <AlertDescription>
                {t('Empty fields keep each channel value unchanged.')}
              </AlertDescription>
            </Alert>

            <SideDrawerSection>
              <SideDrawerSectionHeader title={t('Basic Information')} />

              <div className='grid gap-2'>
                <Label htmlFor='batch-edit-group'>{t('Group')}</Label>
                <MultiSelect
                  id='batch-edit-group'
                  options={groupOptions}
                  selected={toChipValues(form.group)}
                  onChange={(values) => updateForm({ group: values.join(',') })}
                  placeholder={t('Select groups (leave empty to keep current)')}
                />
              </div>

              <div className='grid gap-2'>
                <Label htmlFor='batch-edit-tag'>{t('Tag')}</Label>
                <Input
                  id='batch-edit-tag'
                  value={form.tag ?? ''}
                  onChange={(event) => updateForm({ tag: event.target.value })}
                  placeholder={t('Enter tag name (optional)')}
                />
              </div>

              <div className='grid gap-2'>
                <Label htmlFor='batch-edit-remark'>{t('Remark')}</Label>
                <Input
                  id='batch-edit-remark'
                  value={form.remark ?? ''}
                  onChange={(event) =>
                    updateForm({ remark: event.target.value })
                  }
                  placeholder={t('Enter remark (optional)')}
                />
              </div>
            </SideDrawerSection>

            <SideDrawerSection>
              <SideDrawerSectionHeader title={t('Models')} />

              <div className='grid gap-2'>
                <Label htmlFor='batch-edit-models'>{t('Models')}</Label>
                <MultiSelect
                  id='batch-edit-models'
                  options={modelOptions}
                  selected={toChipValues(form.models)}
                  onChange={(values) => updateForm({ models: values.join(',') })}
                  placeholder={t('Select models or add custom ones')}
                  allowCreate
                  createLabel='Add custom model "{{value}}"'
                  maxVisibleChips={8}
                />
              </div>

              <div className='grid gap-2'>
                <Label htmlFor='batch-edit-model-mapping'>
                  {t('Model Mapping')}
                </Label>
                <Textarea
                  id='batch-edit-model-mapping'
                  value={form.model_mapping ?? ''}
                  onChange={(event) =>
                    updateForm({ model_mapping: event.target.value })
                  }
                  placeholder='{"gpt-4o":"gpt-4o-mini"}'
                  rows={3}
                />
              </div>
            </SideDrawerSection>

            <SideDrawerSection>
              <SideDrawerSectionHeader title={t('Advanced Settings')} />

              <div className='grid gap-4 sm:grid-cols-2'>
                <div className='grid gap-2'>
                  <Label htmlFor='batch-edit-weight'>{t('Weight')}</Label>
                  <Input
                    id='batch-edit-weight'
                    inputMode='numeric'
                    value={form.weight ?? ''}
                    onChange={(event) =>
                      updateForm({ weight: event.target.value })
                    }
                    placeholder='0'
                  />
                </div>

                <div className='grid gap-2'>
                  <Label htmlFor='batch-edit-priority'>{t('Priority')}</Label>
                  <Input
                    id='batch-edit-priority'
                    inputMode='numeric'
                    value={form.priority ?? ''}
                    onChange={(event) =>
                      updateForm({ priority: event.target.value })
                    }
                    placeholder='0'
                  />
                </div>
              </div>

              <div className='grid gap-2'>
                <Label htmlFor='batch-edit-test-model'>{t('Test Model')}</Label>
                <Input
                  id='batch-edit-test-model'
                  value={form.test_model ?? ''}
                  onChange={(event) =>
                    updateForm({ test_model: event.target.value })
                  }
                  placeholder={t('Enter the model used for channel testing')}
                />
              </div>

              <div className='grid gap-2'>
                <Label htmlFor='batch-edit-auto-ban'>{t('Auto Ban')}</Label>
                <Select
                  value={form.auto_ban ?? 'unchanged'}
                  onValueChange={(value) =>
                    updateForm({
                      auto_ban: value as ChannelBatchEditForm['auto_ban'],
                    })
                  }
                  items={autoBanOptions}
                >
                  <SelectTrigger id='batch-edit-auto-ban' className='w-full'>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent alignItemWithTrigger={false}>
                    <SelectGroup>
                      {autoBanOptions.map((option) => (
                        <SelectItem key={option.value} value={option.value}>
                          {option.label}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  </SelectContent>
                </Select>
              </div>
            </SideDrawerSection>

            {failed.length > 0 && (
              <Alert variant='destructive'>
                <AlertTitle>{t('Failed to update channels')}</AlertTitle>
                <AlertDescription>
                  <ul className='space-y-1'>
                    {failed.map((item) => (
                      <li key={item.id}>
                        {t('Channel #{{id}}', { id: item.id })}: {item.reason}
                      </li>
                    ))}
                  </ul>
                </AlertDescription>
              </Alert>
            )}
          </div>

          <SheetFooter className={sideDrawerFooterClassName()}>
            <Button
              variant='outline'
              onClick={() => {
                props.onOpenChange(false)
              }}
            >
              {t('Cancel')}
            </Button>
            <Button onClick={handleSave} disabled={isSubmitting}>
              {isSubmitting && <Loader2 className='animate-spin' />}
              {t('Save')}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      <Dialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title={t('{{count}} channel(s) will be updated', {
          count: props.selectedIds.length,
        })}
        contentHeight='auto'
        bodyClassName='space-y-3'
        footer={
          <>
            <Button variant='outline' onClick={() => setConfirmOpen(false)}>
              {t('Cancel')}
            </Button>
            <Button disabled={isSubmitting} onClick={handleConfirm}>
              {t('Confirm')}
            </Button>
          </>
        }
      >
        <div className='space-y-2'>
          <div className='text-sm font-medium'>{t('Fields to update')}</div>
          <ul className='space-y-1 text-sm'>
            {confirmFields.map((field) => (
              <li key={field.label} title={field.summary}>
                {t(field.label)}: {truncateSummary(field.summary)}
              </li>
            ))}
          </ul>
        </div>
      </Dialog>
    </>
  )
}
