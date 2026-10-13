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
import { Gauge, Loader2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'

import type { MultiKeyConfirmAction } from '../../types'

type MultiKeyTableRowActionsProps = {
  keyIndex: number
  status: number
  canTest: boolean
  testingKeyIndex: number | null
  canDelete: boolean
  onTest: (keyIndex: number) => void
  onAction: (action: MultiKeyConfirmAction) => void
}

export function MultiKeyTableRowActions({
  keyIndex,
  status,
  canTest,
  testingKeyIndex,
  canDelete,
  onTest,
  onAction,
}: MultiKeyTableRowActionsProps) {
  const { t } = useTranslation()
  const isEnabled = status === 1
  const isTesting = testingKeyIndex === keyIndex

  return (
    <div className='flex items-center justify-end gap-2'>
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              variant='ghost'
              size='icon-sm'
              onClick={() => onTest(keyIndex)}
              disabled={!canTest || isTesting}
              aria-label={t('Test Connection')}
              title={
                canTest ? undefined : t('No permission to perform this action')
              }
            />
          }
        >
          {isTesting ? (
            <Loader2 className='size-4 animate-spin' />
          ) : (
            <Gauge className='size-4' />
          )}
        </TooltipTrigger>
        <TooltipContent>{t('Test Connection')}</TooltipContent>
      </Tooltip>
      {isEnabled ? (
        <Button
          variant='outline'
          size='sm'
          onClick={() => onAction({ type: 'disable', keyIndex })}
        >
          {t('Disable')}
        </Button>
      ) : (
        <Button
          variant='outline'
          size='sm'
          onClick={() => onAction({ type: 'enable', keyIndex })}
        >
          {t('Enable')}
        </Button>
      )}
      <Button
        variant='destructive'
        size='sm'
        onClick={() => {
          if (!canDelete) return
          onAction({ type: 'delete', keyIndex })
        }}
        disabled={!canDelete}
        title={
          canDelete ? undefined : t('No permission to perform this action')
        }
      >
        {t('Delete')}
      </Button>
    </div>
  )
}
