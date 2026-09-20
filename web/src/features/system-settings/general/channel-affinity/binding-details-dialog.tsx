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
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { getChannelAffinityBindings } from './api'
import type { ChannelAffinityBinding } from './types'

interface Props { open: boolean; onOpenChange: (open: boolean) => void }

export function BindingDetailsDialog({ open, onOpenChange }: Props) {
  const { t } = useTranslation()
  const [status, setStatus] = useState<'loading' | 'ready' | 'empty' | 'error'>('loading')
  const [bindings, setBindings] = useState<ChannelAffinityBinding[]>([])
  const requestId = useRef(0)

  useEffect(() => {
    if (!open) {
      requestId.current += 1
      setBindings([])
      setStatus('loading')
      return
    }
    const id = ++requestId.current
    setStatus('loading')
    getChannelAffinityBindings().then((res) => {
      if (id !== requestId.current) return
      if (!res.success) { setStatus('error'); return }
      const data = (res.data ?? []).map((binding) => {
        const seen = new Set<number>()
        const tokens = binding.tokens.filter((token) => {
          if (seen.has(token.token_id)) return false
          seen.add(token.token_id)
          return true
        }).sort((a, b) => a.token_id - b.token_id)
        return { ...binding, tokens }
      })
      setBindings(data)
      setStatus(data.length ? 'ready' : 'empty')
    }).catch(() => {
      if (id === requestId.current) setStatus('error')
    })
  }, [open])

  const close = () => onOpenChange(false)
  return (
    <Dialog open={open} onOpenChange={onOpenChange} title={t('Channel Affinity Bindings')} contentClassName='sm:max-w-2xl' footer={<Button variant='outline' onClick={close}>{t('Close')}</Button>}>
      {status === 'loading' && <div role='status' className='py-8 text-center text-muted-foreground'>{t('Loading bindings...')}</div>}
      {status === 'error' && <div role='alert' className='py-8 text-center text-destructive'>{t('Failed to load bindings')}</div>}
      {status === 'empty' && <div className='py-8 text-center text-muted-foreground'>{t('No bindings')}</div>}
      {status === 'ready' && <div className='grid gap-3'>
        {bindings.map((binding) => <div key={binding.channel_id} className='grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] gap-3 border-b pb-2 last:border-0'>
          <div className='font-medium'>{binding.channel_name || '-'}</div>
          <div className='grid gap-1'>{binding.tokens.map((token) => <div key={token.token_id}>{`${token.token_name || '-'}（${token.token_id}）`}</div>)}</div>
        </div>)}
      </div>}
    </Dialog>
  )
}
