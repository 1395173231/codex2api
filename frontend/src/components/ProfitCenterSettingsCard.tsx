import { useEffect, useState } from 'react'
import { CircleDollarSign, Loader2, Save } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import type { ProfitSettings } from '../types'
import { getErrorMessage } from '../utils/error'
import { useToast } from '../hooks/useToast'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'

const PPM = 1_000_000

export default function ProfitCenterSettingsCard() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [settings, setSettings] = useState<ProfitSettings | null>(null)
  const [ratio, setRatio] = useState('1')
  const [multiplier, setMultiplier] = useState('1')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    let cancelled = false
    void api.getProfitSettings()
      .then((value) => {
        if (cancelled) return
        setSettings(value)
        setRatio(String(value.default_settlement_ratio_ppm / PPM))
        setMultiplier(String(value.default_group_multiplier_ppm / PPM))
      })
      .catch((error) => {
        if (!cancelled) showToast(getErrorMessage(error), 'error')
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => { cancelled = true }
  }, [showToast])

  const save = async (enabled = settings?.enabled ?? false) => {
    const ratioNumber = Number(ratio)
    const multiplierNumber = Number(multiplier)
    if (!Number.isFinite(ratioNumber) || ratioNumber <= 0 || !Number.isFinite(multiplierNumber) || multiplierNumber <= 0) {
      showToast(t('profit.settings.invalidNumber'), 'error')
      return
    }
    setSaving(true)
    try {
      const updated = await api.updateProfitSettings({
        enabled,
        default_settlement_ratio_ppm: Math.round(ratioNumber * PPM),
        default_group_multiplier_ppm: Math.round(multiplierNumber * PPM),
      })
      setSettings(updated)
      setRatio(String(updated.default_settlement_ratio_ppm / PPM))
      setMultiplier(String(updated.default_group_multiplier_ppm / PPM))
      showToast(t('profit.settings.saved'), 'success')
      window.dispatchEvent(new CustomEvent('profit-settings-changed', { detail: updated }))
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card className="overflow-hidden">
      <CardContent className="p-0">
        <div className="flex flex-col gap-4 border-b border-border/70 p-5 sm:flex-row sm:items-start sm:justify-between">
          <div className="flex gap-3">
            <span className="flex size-10 shrink-0 items-center justify-center rounded-xl bg-emerald-500/10 text-emerald-600 dark:text-emerald-400">
              <CircleDollarSign className="size-5" />
            </span>
            <div>
              <h3 className="font-semibold text-foreground">{t('profit.settings.title')}</h3>
              <p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{t('profit.settings.description')}</p>
            </div>
          </div>
          {loading ? <Loader2 className="size-5 animate-spin text-muted-foreground" /> : (
            <div className="flex items-center gap-3 rounded-xl border border-border bg-muted/30 px-3 py-2">
              <span className="text-sm font-medium">{settings?.enabled ? t('common.enabled') : t('common.disabled')}</span>
              <Switch
                checked={settings?.enabled ?? false}
                disabled={saving}
                onCheckedChange={(checked) => {
                  setSettings((current) => current ? { ...current, enabled: checked } : current)
                  void save(checked)
                }}
              />
            </div>
          )}
        </div>
        <div className="grid gap-4 p-5 lg:grid-cols-2">
          <label className="space-y-2">
            <span className="text-sm font-semibold text-foreground">{t('profit.settings.ratio')}</span>
            <Input type="number" min="0.000001" step="0.01" value={ratio} onChange={(event) => setRatio(event.target.value)} />
            <span className="block text-xs leading-relaxed text-muted-foreground">{t('profit.settings.ratioHelp')}</span>
          </label>
          <label className="space-y-2">
            <span className="text-sm font-semibold text-foreground">{t('profit.settings.defaultMultiplier')}</span>
            <Input type="number" min="0.000001" step="0.01" value={multiplier} onChange={(event) => setMultiplier(event.target.value)} />
            <span className="block text-xs leading-relaxed text-muted-foreground">{t('profit.settings.multiplierHelp')}</span>
          </label>
        </div>
        <div className="flex justify-end border-t border-border/70 px-5 py-4">
          <Button disabled={loading || saving || !settings} onClick={() => void save()}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
            {t('profit.settings.save')}
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}
