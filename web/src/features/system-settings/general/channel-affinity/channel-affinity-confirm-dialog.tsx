/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: ReactNode
  desc: ReactNode
  handleConfirm: () => void
  destructive?: boolean
  confirmDisabled?: boolean
}

export function ChannelAffinityConfirmDialog(props: Props) {
  const { t } = useTranslation()

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={props.title}
      contentClassName='sm:max-w-md'
      contentHeight='auto'
      bodyClassName='flex items-start'
      footer={
        <>
          <Button variant='outline' onClick={() => props.onOpenChange(false)}>
            {t('Cancel')}
          </Button>
          <Button
            variant={props.destructive ? 'destructive' : 'default'}
            onClick={props.handleConfirm}
            disabled={props.confirmDisabled}
          >
            {t('Continue')}
          </Button>
        </>
      }
    >
      <div className='text-muted-foreground text-sm'>{props.desc}</div>
    </Dialog>
  )
}
