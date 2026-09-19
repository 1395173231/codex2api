import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Info, RefreshCw, Save } from 'lucide-react'
import { api } from '../api'
import type { CodexTurnStateSettings as Settings } from '../types'
import { Button } from './ui/button'
import { Input } from './ui/input'
import { Switch } from './ui/switch'
import { getErrorMessage } from '../utils/error'
import { parseTurnStateModels, parseTurnStateTargetLengths, validTurnStateModels, validTurnStateTargetLengths } from '../lib/codexTurnStateStatus'

const numberFields = [
  ['ttl_seconds', 180, 3600],
  ['refresh_before_seconds', 1, 3599], ['retry_interval_seconds', 1, 3600],
  ['attempt_timeout_seconds', 1, 120], ['max_attempts', 1, 10], ['concurrency', 1, 16],
] as const

export default function CodexTurnStateSettings() {
  const { t } = useTranslation()
  const [config, setConfig] = useState<Settings | null>(null)
  const [models, setModels] = useState('')
  const [targetLengths, setTargetLengths] = useState('')
  const [clearProxy, setClearProxy] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [saved, setSaved] = useState(false)
  const [reload, setReload] = useState(0)
  useEffect(() => {
    let active = true
    setError('')
    api.getCodexTurnStateSettings().then(result => {
      if (active) { setConfig(result); setModels(result.models.join(', ')); setTargetLengths(result.target_lengths.join(', ')); setClearProxy(false) }
    }).catch(err => { if (active) setError(getErrorMessage(err)) })
    return () => { active = false }
  }, [reload])
  const change = (patch: Partial<Settings>) => { setSaved(false); setConfig(current => current ? { ...current, ...patch } : current) }
  const save = async () => {
    if (!config || busy) return
    setBusy(true); setError(''); setSaved(false)
    try {
      const normalized = parseTurnStateModels(models)
      const normalizedTargetLengths = parseTurnStateTargetLengths(targetLengths)
      if (!validTurnStateModels(normalized)) throw new Error(t('turnState.invalidModels'))
      if (!validTurnStateTargetLengths(normalizedTargetLengths)) throw new Error(t('turnState.invalidNumbers'))
      if (config.enabled && (clearProxy || (!config.proxy_configured && !config.harvest_proxy_url.trim()))) throw new Error(t('turnState.configRequired'))
      if (numberFields.some(([key, min, max]) => !Number.isInteger(config[key]) || config[key] < min || config[key] > max) || config.refresh_before_seconds >= config.ttl_seconds) throw new Error(t('turnState.invalidNumbers'))
      const result = await api.updateCodexTurnStateSettings({ ...config, models: normalized, target_lengths: normalizedTargetLengths, clear_proxy: clearProxy })
      setConfig(result); setModels(result.models.join(', ')); setTargetLengths(result.target_lengths.join(', ')); setClearProxy(false); setSaved(true)
    } catch (err) { setError(getErrorMessage(err)) }
    finally { setBusy(false) }
  }
  return <section className="min-w-0 space-y-4 border-t border-border py-4">
    <div className="flex items-center justify-between gap-3">
      <h3 className="text-sm font-semibold">{t('turnState.settingsTitle')}</h3>
      <span title={t('turnState.caveat')} tabIndex={0} aria-label={t('turnState.caveat')}><Info className="size-4 text-muted-foreground" /></span>
    </div>
    {error && <div className="flex items-center gap-2"><p role="alert" className="min-w-0 break-words text-xs text-destructive">{error}</p>{!config && <Button type="button" variant="ghost" size="icon-sm" title={t('turnState.reload')} aria-label={t('turnState.reload')} onClick={() => setReload(value => value + 1)}><RefreshCw className="size-4" /></Button>}</div>}
    {!config && !error && <p className="text-xs text-muted-foreground">{t('common.loading')}</p>}
    {config && <>
      <div className="flex items-center justify-between gap-3"><label htmlFor="turn-state-enabled" className="text-sm">{t('turnState.enabled')}</label><Switch id="turn-state-enabled" disabled={busy} checked={config.enabled} onCheckedChange={enabled => change({ enabled })} /></div>
      <p className="text-xs text-muted-foreground">{t('turnState.collectionCostHint')}</p>
      <div className="flex items-center justify-between gap-3"><label htmlFor="turn-state-scheduling-enabled" className="text-sm">{t('turnState.schedulingEnabled')}</label><Switch id="turn-state-scheduling-enabled" disabled={busy || !config.enabled} checked={config.scheduling_enabled} onCheckedChange={scheduling_enabled => change({ scheduling_enabled })} /></div>
      <p className="text-xs text-muted-foreground">{t('turnState.schedulingHint')}</p>
      <div className="grid gap-3 sm:grid-cols-2">
        <label className="space-y-1.5 text-xs sm:col-span-2"><span>{t('turnState.models')}</span><Input disabled={busy} value={models} onChange={event => { setModels(event.target.value); setSaved(false) }} placeholder="gpt-5.5, gpt-5.4" autoComplete="off" spellCheck={false} /></label>
        <label className="space-y-1.5 text-xs sm:col-span-2"><span>{t('turnState.config.target_lengths')}</span><Input disabled={busy} value={targetLengths} onChange={event => { setTargetLengths(event.target.value); setSaved(false) }} placeholder="292, 332" inputMode="numeric" autoComplete="off" spellCheck={false} /></label>
        <label className="space-y-1.5 text-xs sm:col-span-2"><span>{t('turnState.proxy')}</span><Input type="password" disabled={busy || clearProxy} value={config.harvest_proxy_url} onChange={event => change({ harvest_proxy_url: event.target.value })} autoComplete="off" spellCheck={false} /><span className="block text-muted-foreground">{t(config.proxy_configured ? 'turnState.proxyConfigured' : 'turnState.proxyMissing')}</span></label>
        <div className="flex items-center justify-between gap-3 sm:col-span-2"><label htmlFor="turn-state-clear-proxy" className="text-xs">{t('turnState.clearProxy')}</label><Switch id="turn-state-clear-proxy" disabled={busy || !config.proxy_configured} checked={clearProxy} onCheckedChange={value => { setClearProxy(value); setSaved(false) }} /></div>
        {numberFields.map(([key, min, max]) => <label key={key} className="space-y-1.5 text-xs"><span>{t('turnState.config.' + key)}</span><Input type="number" min={min} max={key === 'refresh_before_seconds' ? config.ttl_seconds - 1 : max} step={1} disabled={busy} value={Number.isFinite(config[key]) ? config[key] : ''} onChange={event => change({ [key]: event.target.value === '' ? NaN : Number(event.target.value) })} /></label>)}
      </div>
      <div className="flex flex-wrap items-center gap-3"><Button type="button" disabled={busy} onClick={() => void save()}><Save className="size-4" />{t('turnState.saveConfig')}</Button>{saved && <span role="status" className="text-xs text-muted-foreground">{t('turnState.saved')}</span>}</div>
    </>}
  </section>
}
