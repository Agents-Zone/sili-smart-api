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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { X } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'

import {
  clearBindingCache,
  getChannelAffinityBindings,
} from './api'
import { ChannelAffinityConfirmDialog } from './channel-affinity-confirm-dialog'
import type { ChannelAffinityBinding, ChannelAffinityToken } from './types'

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCacheCleared?: () => void
}

interface ClearTarget {
  binding: ChannelAffinityBinding
  token: ChannelAffinityToken
}

export function BindingDetailsDialog(props: Props) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [clearTarget, setClearTarget] = useState<ClearTarget | null>(null)

  useEffect(() => {
    if (!props.open) setClearTarget(null)
  }, [props.open])

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

  const clearMutation = useMutation({
    mutationFn: (keySuffix: string) => clearBindingCache(keySuffix),
    onSuccess: async (res) => {
      if (!res.success) {
        toast.error(res.message || t('Failed to clear'))
        return
      }
      toast.success(t('Cleared'))
      setClearTarget(null)
      await queryClient.invalidateQueries({
        queryKey: ['channel-affinity-bindings'],
      })
      props.onCacheCleared?.()
    },
    onError: () => toast.error(t('Failed to clear')),
  })

  const close = () => props.onOpenChange(false)
  return (
    <>
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
                      key={token.key_suffix}
                      className='flex items-center justify-between gap-2'
                    >
                      <span>{`${token.token_name || '-'}（${token.token_id}）${
                        token.model_name ? ` · ${token.model_name}` : ''
                      }`}</span>
                      <Button
                        variant='ghost'
                        size='icon'
                        className='h-7 w-7 shrink-0'
                        onClick={() =>
                          setClearTarget({ binding, token })
                        }
                        title={t('Clear this binding')}
                        aria-label={t('Clear this binding')}
                      >
                        <X className='h-3 w-3' />
                      </Button>
                    </div>
                  ))}
                </div>
              </div>
            ))}
          </div>
        )}
      </Dialog>

      <ChannelAffinityConfirmDialog
        open={clearTarget !== null}
        onOpenChange={(v) => !v && setClearTarget(null)}
        title={t('Confirm clearing this binding')}
        desc={`${t('Token')}: ${clearTarget?.token.token_name || '-'}（${
          clearTarget?.token.token_id ?? '-'
        }）${clearTarget?.token.model_name ? ` · ${clearTarget?.token.model_name}` : ''} · ${t(
          'Channel'
        )}: ${clearTarget?.binding.channel_name || '-'}`}
        handleConfirm={() =>
          clearTarget && clearMutation.mutate(clearTarget.token.key_suffix)
        }
        destructive
        confirmDisabled={clearMutation.isPending}
      />
    </>
  )
}
