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
import { useQuery } from '@tanstack/react-query'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import {
  sideDrawerContentClassName,
  sideDrawerFooterClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
  SideDrawerSection,
  SideDrawerSectionHeader,
} from '@/components/drawer-layout'
import { MultiSelect } from '@/components/multi-select'
import { Alert, AlertDescription } from '@/components/ui/alert'
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
  type BatchEditBuildResult,
  type ChannelBatchEditForm,
} from '../../lib'

type ChannelBatchEditDrawerProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  selectedIds: number[]
  onCommitted?: () => void
}

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
  const [form, setForm] = useState<ChannelBatchEditForm>({})
  // Validated payload of the last save click, consumed by the confirmation dialog.
  const pendingCommitRef = useRef<BatchEditBuildResult | null>(null)

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
    pendingCommitRef.current = null
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

  const handleSave = () => {
    const result = buildBatchEditPayload(props.selectedIds, form)

    if (result.error) {
      toast.warning(t(result.error))
      return
    }

    pendingCommitRef.current = result
  }

  return (
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
                onChange={(event) => updateForm({ remark: event.target.value })}
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
        </div>

        <SheetFooter className={sideDrawerFooterClassName()}>
          <Button
            variant='outline'
            onClick={() => {
              pendingCommitRef.current = null
              props.onOpenChange(false)
            }}
          >
            {t('Cancel')}
          </Button>
          <Button onClick={handleSave}>{t('Save')}</Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}
