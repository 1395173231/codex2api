import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Info, RefreshCw, Save, Trash2 } from 'lucide-react'
import { api } from '../api'
import type { CodexTurnStateStatus } from '../types'
import { Button } from './ui/button'
import { Input } from './ui/input'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime } from '../utils/time'
import { formatCodexTurnStateCountdown } from '../lib/codexTurnState'
import { turnStateDisplayStatus, turnStateRemainingMs } from '../lib/codexTurnStateStatus'

export function CodexTurnStateSummary({ items }: { items?: CodexTurnStateStatus[] }) {
  const { t } = useTranslation()
  if (!items?.length) return null
  return <div className="flex flex-wrap gap-x-2 gap-y-1 text-[10px] text-muted-foreground">
    {items.map(item => <span key={item.model} className="break-all" title={item.last_error || undefined}>
      {item.model}: {t('turnState.status.' + turnStateDisplayStatus(item))}
    </span>)}
  </div>
}

export default function CodexTurnStatePanel({ accountId }: { accountId: number }) {
  const { t } = useTranslation()
  const [data, setData] = useState<{ enabled: boolean; items: CodexTurnStateStatus[] } | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [model, setModel] = useState('')
  const [token, setToken] = useState('')
  const [now, setNow] = useState(Date.now)
  const generationRef = useRef(0)
  useEffect(() => {
    const controller = new AbortController()
    generationRef.current += 1
    setBusy(false)
    let fetching = false
    setData(null)
    setError('')
    setNotice('')
    setModel('')
    setToken('')
    const load = async () => {
      if (fetching) return
      fetching = true
      try {
        const result = await api.getAccountTurnStates(accountId, controller.signal)
        if (!controller.signal.aborted) { setData(result); setError('') }
      } catch (err) {
        if (!controller.signal.aborted) setError(getErrorMessage(err))
      } finally { fetching = false }
    }
    void load()
    const poll = window.setInterval(() => void load(), 15000)
    const clock = window.setInterval(() => setNow(Date.now()), 1000)
    return () => { generationRef.current += 1; controller.abort(); window.clearInterval(poll); window.clearInterval(clock) }
  }, [accountId])
  const run = async (action: () => Promise<unknown>, clearToken = false, queued = false) => {
    if (busy) return
    const generation = generationRef.current
    setBusy(true); setError(''); setNotice('')
    try {
      await action()
      if (generation !== generationRef.current) return
      if (clearToken) setToken('')
      setNotice(t(queued ? 'turnState.queued' : 'turnState.saved'))
      const result = await api.getAccountTurnStates(accountId)
      if (generation === generationRef.current) setData(result)
    } catch (err) { if (generation === generationRef.current) setError(getErrorMessage(err)) }
    finally { if (generation === generationRef.current) setBusy(false) }
  }
  return <div className="min-w-0 space-y-3">
    <div className="flex items-center justify-between gap-2">
      <h3 className="text-sm font-semibold">{t('turnState.title')}</h3>
      <span title={t('turnState.caveat')} tabIndex={0} aria-label={t('turnState.caveat')}><Info className="size-4 text-muted-foreground" /></span>
    </div>
    {error && <p role="alert" className="break-words text-xs text-destructive">{error}</p>}
    {notice && <p role="status" className="text-xs text-muted-foreground">{notice}</p>}
    {!data && !error && <p className="text-xs text-muted-foreground">{t('common.loading')}</p>}
    {data && <>
      {!data.enabled && <p className="text-xs text-muted-foreground">{t('turnState.disabled')}</p>}
      <div className="overflow-x-auto">
        <table className="w-full min-w-[380px] text-left text-xs">
          <thead><tr className="border-b text-muted-foreground">
            <th className="min-w-20 py-2 pr-2">{t('turnState.model')}</th>
            <th className="py-2 pr-2">{t('turnState.state')}</th>
            <th className="py-2 pr-2">{t('turnState.validity')}</th>
            <th className="w-20 py-2"><span className="sr-only">{t('turnState.actions')}</span></th>
          </tr></thead>
          <tbody>{data.items.map(item => <tr key={item.model} className="border-b border-border/50 align-top">
            <td className="min-w-20 max-w-40 break-all py-2 pr-2 font-mono">{item.model}
              <div className="mt-1 whitespace-nowrap font-sans text-muted-foreground">{item.token_length} / {item.target_length}</div>
            </td>
            <td className="max-w-48 py-2 pr-2">
              <span className={item.ready && turnStateRemainingMs(item, now) > 0 ? 'text-emerald-600 dark:text-emerald-400' : 'text-muted-foreground'}>{t('turnState.status.' + turnStateDisplayStatus(item, now))}</span>
              <div className="mt-1 text-muted-foreground">{t('turnState.attempts', { count: item.attempts })}</div>
              {item.last_attempt_at && <div title={t('turnState.lastAttempt')} className="mt-1 text-muted-foreground">{formatBeijingTime(item.last_attempt_at)}</div>}
              {item.next_attempt_at && <div className="mt-1 text-muted-foreground">{t('turnState.nextAttempt')}: {formatBeijingTime(item.next_attempt_at)}</div>}
              {item.last_error && <p className="mt-1 break-words text-destructive">{item.last_error}</p>}
            </td>
            <td className="py-2 pr-2 tabular-nums">
              {item.expires_at ? <span title={formatBeijingTime(item.expires_at)}>{formatCodexTurnStateCountdown(turnStateRemainingMs(item, now))}</span> : '-'}
              {item.issued_at && <div className="mt-1 text-muted-foreground" title={t('turnState.issuedAt')}>{formatBeijingTime(item.issued_at)}</div>}
            </td>
            <td className="py-2"><div className="flex gap-1">
              <Button type="button" size="icon-sm" variant="ghost" title={t('turnState.collect')} aria-label={t('turnState.collect')} disabled={busy || !data.enabled || item.status === 'refreshing'} onClick={() => void run(() => api.refreshAccountTurnState(accountId, item.model), false, true)}><RefreshCw className="size-4" /></Button>
              <Button type="button" size="icon-sm" variant="ghost" title={t('turnState.remove')} aria-label={t('turnState.remove')} disabled={busy || !item.token_length} onClick={() => void run(() => api.deleteAccountTurnState(accountId, item.model))}><Trash2 className="size-4" /></Button>
            </div></td>
          </tr>)}</tbody>
        </table>
      </div>
      {data.items.length === 0 && <p className="text-xs text-muted-foreground">{t('turnState.empty')}</p>}
      <div className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_auto]">
        <Input aria-label={t('turnState.model')} placeholder={t('turnState.model')} value={model} onChange={event => setModel(event.target.value)} disabled={busy} autoComplete="off" />
        <Button type="button" variant="outline" disabled={busy || !data.enabled || !model.trim()} onClick={() => void run(() => api.refreshAccountTurnState(accountId, model.trim()), false, true)}><RefreshCw className="size-4" />{t('turnState.collect')}</Button>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Input className="min-w-0 flex-1 font-mono text-xs" type="password" aria-label={t('turnState.token')} placeholder={t('turnState.token')} value={token} onChange={event => setToken(event.target.value)} disabled={busy} autoComplete="off" spellCheck={false} />
        <Button type="button" variant="outline" disabled={busy || !model.trim() || !token.trim()} onClick={() => void run(() => api.saveAccountTurnState(accountId, model.trim(), token.trim()), true)}><Save className="size-4" />{t('turnState.saveToken')}</Button>
      </div>
    </>}
  </div>
}
