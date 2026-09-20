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
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'

import { getChannelAffinityBindings } from './api'
import type { ChannelAffinityBinding } from './types'

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function BindingDetailsDialog(props: Props) {
  const { t } = useTranslation()
  const bindingsQuery = useQuery<ChannelAffinityBinding[]>({
    queryKey: ['channel-affinity-bindings'],
    queryFn: async () => {
      const response = await getChannelAffinityBindings()
      if (!response.success) {
        throw new Error(response.message || 'Failed to load bindings')
      }
      return response.data ?? []
    },
    enabled: props.open,
  })
  const bindings = bindingsQuery.data ?? []

  const close = () => props.onOpenChange(false)
  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Channel Affinity Bindings')}
      contentClassName='sm:max-w-2xl'
      footer={
        <Button variant='outline' onClick={close}>
          {t('Close')}
        </Button>
      }
    >
      {bindingsQuery.isPending && (
        <div role='status' className='text-muted-foreground py-8 text-center'>
          {t('Loading bindings...')}
        </div>
      )}
      {bindingsQuery.isError && (
        <div role='alert' className='text-destructive py-8 text-center'>
          {t('Failed to load bindings')}
        </div>
      )}
      {!bindingsQuery.isPending && !bindingsQuery.isError && bindings.length === 0 && (
        <div className='text-muted-foreground py-8 text-center'>
          {t('No bindings')}
        </div>
      )}
      {!bindingsQuery.isPending && !bindingsQuery.isError && bindings.length > 0 && (
        <div className='grid gap-3'>
          {bindings.map((binding) => (
            <div
              key={binding.channel_id}
              className='grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] gap-3 border-b pb-2 last:border-0'
            >
              <div className='font-medium'>{binding.channel_name || '-'}</div>
              <div className='grid gap-1'>
                {binding.tokens.map((token) => (
                  <div
                    key={token.token_id}
                  >{`${token.token_name || '-'}（${token.token_id}）`}</div>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}
    </Dialog>
  )
}
