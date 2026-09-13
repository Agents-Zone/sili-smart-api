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
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { truncateText } from '@/lib/utils'

import type { BatchEditField } from '../../lib'
import type { BatchUpdateParams } from '../../types'

type BatchEditConfirmDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  selectedCount: number
  payload: BatchUpdateParams | null
  fields: BatchEditField[]
  isSubmitting: boolean
  onConfirm: () => void
}

/** Overlong confirmation summaries are cut here and suffixed with an ellipsis (spec 4.3.5). */
const MAX_SUMMARY_LENGTH = 50

/** Summary of one effective field: the value written into the payload. */
function summarize(
  field: BatchEditField,
  payload: BatchUpdateParams,
  t: (key: string) => string
): { display: string; full: string } {
  const value = payload[field.key]
  if (value === undefined) return { display: '', full: '' }
  if (field.key === 'auto_ban')
    return { display: t(value === 1 ? 'Enabled' : 'Disabled') }
  const full = String(value)
  return { display: truncateText(full, MAX_SUMMARY_LENGTH), full }
}

export function BatchEditConfirmDialog(props: BatchEditConfirmDialogProps) {
  const { t } = useTranslation()

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('{{count}} channel(s) will be updated', {
        count: props.selectedCount,
      })}
      contentHeight='auto'
      bodyClassName='space-y-3'
      footer={
        <>
          <Button variant='outline' onClick={() => props.onOpenChange(false)}>
            {t('Cancel')}
          </Button>
          <Button disabled={props.isSubmitting} onClick={props.onConfirm}>
            {t('Confirm')}
          </Button>
        </>
      }
    >
      <div className='space-y-2'>
        <div className='text-sm font-medium'>{t('Fields to update')}</div>
        <ul className='space-y-1 text-sm'>
          {props.fields.map((field) => {
            const summary = props.payload
              ? summarize(field, props.payload, t)
              : { display: '', full: '' }
            return (
              // spec 4.3.5: display truncates at 50 chars, title hover shows the full value
              <li key={field.label} title={summary.full}>
                {t(field.label)}: {summary.display}
              </li>
            )
          })}
        </ul>
      </div>
    </Dialog>
  )
}
