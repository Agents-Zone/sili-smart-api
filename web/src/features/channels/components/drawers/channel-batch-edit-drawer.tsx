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
import { zodResolver } from '@hookform/resolvers/zod'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2 } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import {
  sideDrawerContentClassName,
  sideDrawerFooterClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Form } from '@/components/ui/form'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'

import { getAllModels, getGroups } from '../../api'
import {
  buildBatchEditSubmission,
  channelBatchEditSchema,
  handleBatchUpdate,
  validateBatchEditTargets,
  type BatchEditField,
  type ChannelBatchEditFormValues,
} from '../../lib'
import type { BatchUpdateOutcome } from '../../lib'
import type { BatchUpdateParams } from '../../types'
import { BatchEditConfirmDialog } from './channel-batch-edit-confirm-dialog'
import { ChannelBatchEditFields } from './channel-batch-edit-fields'

type ChannelBatchEditDrawerProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  selectedIds: number[]
  onCommitted?: () => void
  /** Submit implementation, defaults to handleBatchUpdate; tests inject a stub. */
  submit?: (payload: BatchUpdateParams) => Promise<BatchUpdateOutcome>
}

export function ChannelBatchEditDrawer(props: ChannelBatchEditDrawerProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [pending, setPending] = useState<{
    payload: BatchUpdateParams
    fields: BatchEditField[]
  } | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [outcome, setOutcome] = useState<BatchUpdateOutcome | null>(null)

  const form = useForm<ChannelBatchEditFormValues>({
    resolver: zodResolver(channelBatchEditSchema),
    defaultValues: { auto_ban: 'unchanged' },
  })

  // 抽屉随批量工具条常驻挂载，未打开时不预取全量模型列表
  const { data: groupsData } = useQuery({
    queryKey: ['groups'],
    queryFn: getGroups,
    enabled: props.open,
  })

  const { data: allModelsData } = useQuery({
    queryKey: ['channel_models'],
    queryFn: getAllModels,
    enabled: props.open,
  })

  useEffect(() => {
    if (!props.open) return
    form.reset({ auto_ban: 'unchanged' })
    setPending(null)
    setConfirmOpen(false)
    setIsSubmitting(false)
    setOutcome(null)
  }, [props.open, form])

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

  const onSubmit = (values: ChannelBatchEditFormValues) => {
    const submission = buildBatchEditSubmission(props.selectedIds, values)
    if (!submission.payload) {
      // No effective field: warn instead of opening the dialog.
      toast.warning(t('You must fill in at least one field'))
      return
    }
    setOutcome(null)
    setPending({ payload: submission.payload, fields: submission.fields })
    setConfirmOpen(true)
  }

  const handleSave = () => {
    if (isSubmitting) return

    const targetIssue = validateBatchEditTargets(props.selectedIds)
    if (targetIssue) {
      toast.warning(t(targetIssue))
      return
    }

    // Field level issues are rendered by FormMessage under each input.
    void form.handleSubmit(onSubmit)()
  }

  const defaultSubmit = (payload: BatchUpdateParams) =>
    handleBatchUpdate(payload, queryClient, props.onCommitted)

  const handleConfirm = async () => {
    if (!pending || isSubmitting) return

    const submit = props.submit ?? defaultSubmit
    setConfirmOpen(false)
    setIsSubmitting(true)

    let failed: BatchUpdateOutcome | null = null
    try {
      const result = await submit(pending.payload)
      if (result.ok) {
        // Success closes the drawer; the upstream selection is cleared via onCommitted.
        props.onOpenChange(false)
        return
      }
      failed = result
    } catch (error) {
      // 提交实现抛错时给兜底提示：失败明细为空时告警块不渲染，静默失败会让用户无从下手
      toast.error(
        error instanceof Error && error.message
          ? error.message
          : t('Failed to update channels')
      )
      failed = { ok: false, count: 0, failed: [] }
    } finally {
      setIsSubmitting(false)
    }

    // Failure keeps the drawer and the filled values so the user can retry.
    setOutcome(failed)
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

            {outcome && !outcome.ok && outcome.failed.length > 0 && (
              <Alert variant='destructive'>
                <AlertTitle>{t('Failed to update channels')}</AlertTitle>
                <AlertDescription>
                  {/* 服务端已本地化的回滚说明（如「已全部回滚」） */}
                  {outcome.message && <p>{outcome.message}</p>}
                  <ul className='space-y-1'>
                    {outcome.failed.map((item) => (
                      <li key={item.id}>
                        {t('Channel #{{id}}', { id: item.id })}: {item.reason}
                      </li>
                    ))}
                  </ul>
                </AlertDescription>
              </Alert>
            )}

            <Form {...form}>
              <form
                onSubmit={(event) => {
                  // 回车提交与「保存」同路径：同样先校验勾选边界
                  event.preventDefault()
                  handleSave()
                }}
                className='space-y-6'
                noValidate
              >
                <ChannelBatchEditFields
                  form={form}
                  groupOptions={groupOptions}
                  modelOptions={modelOptions}
                />
              </form>
            </Form>
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

      <BatchEditConfirmDialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        selectedCount={props.selectedIds.length}
        payload={pending?.payload ?? null}
        fields={pending?.fields ?? []}
        isSubmitting={isSubmitting}
        onConfirm={handleConfirm}
      />
    </>
  )
}
