import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  Calculator,
  CheckCircle2,
  CircleDollarSign,
  Download,
  FileClock,
  EyeOff,
  Loader2,
  RefreshCw,
  Save,
  Settings2,
  Trash2,
  TriangleAlert,
  Users,
} from 'lucide-react'
import { api } from '../api'
import type {
  ProfitDashboardDimension,
  ProfitDashboardResponse,
  ProfitGroupSetting,
  ProfitPendingAccount,
  ProfitSettings,
  ProfitSettlementRun,
} from '../types'
import { getErrorMessage } from '../utils/error'
import { useToast } from '../hooks/useToast'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { cn } from '@/lib/utils'

const PPM = 1_000_000
type ProfitTab = 'dashboard' | 'pending' | 'groups' | 'settlements'
type DimensionKey = 'groups' | 'api_keys' | 'accounts' | 'models'
type ProfitLoadRange = { startDate: string; endDate: string }
type IgnoreDialogState = {
  account: ProfitPendingAccount
  purge: boolean
  stage: 'options' | 'confirm-purge'
  confirmation: string
}

const PURGE_PROFIT_ACCOUNT_CONFIRM = 'PURGE-PROFIT-ACCOUNT-DATA'
const PURGE_CONFIRMATION_TEXT = '永久删除'

function beijingDate(offsetDays = 0) {
  const now = new Date(Date.now() + offsetDays * 86400000)
  return new Intl.DateTimeFormat('en-CA', {
    timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
  }).format(now)
}

function addDays(date: string, days: number) {
  const value = new Date(`${date}T00:00:00+08:00`)
  value.setUTCDate(value.getUTCDate() + days)
  return new Intl.DateTimeFormat('en-CA', {
    timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
  }).format(value)
}

function startOfWeek(date: string) {
  const value = new Date(`${date}T00:00:00+08:00`)
	const weekdayLabel = new Intl.DateTimeFormat('en-US', { timeZone: 'Asia/Shanghai', weekday: 'short' }).format(value)
	const weekday = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'].indexOf(weekdayLabel)
  return addDays(date, -(weekday === 0 ? 6 : weekday - 1))
}

function formatCNY(micros: number) {
  return new Intl.NumberFormat('zh-CN', { style: 'currency', currency: 'CNY', minimumFractionDigits: 2, maximumFractionDigits: 2 })
    .format(micros / PPM)
}

function formatUSD(micros: number) {
  return new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2 })
    .format(micros / PPM)
}

function formatNumber(value: number) {
  return new Intl.NumberFormat('zh-CN', { notation: value >= 1_000_000 ? 'compact' : 'standard', maximumFractionDigits: 2 }).format(value)
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  URL.revokeObjectURL(url)
}

export default function ProfitCenter() {
  const { showToast } = useToast()
  const today = beijingDate()
  const [settings, setSettings] = useState<ProfitSettings | null>(null)
  const [dashboard, setDashboard] = useState<ProfitDashboardResponse | null>(null)
  const [groups, setGroups] = useState<ProfitGroupSetting[]>([])
  const [pending, setPending] = useState<ProfitPendingAccount[]>([])
  const [settlements, setSettlements] = useState<ProfitSettlementRun[]>([])
  const [activeTab, setActiveTab] = useState<ProfitTab>('dashboard')
  const [dimension, setDimension] = useState<DimensionKey>('groups')
  const [startDate, setStartDate] = useState(startOfWeek(today))
  const [endDate, setEndDate] = useState(addDays(today, 1))
	const [ratio, setRatio] = useState('')
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [pendingSelections, setPendingSelections] = useState<Record<number, string>>({})
  const [groupMultipliers, setGroupMultipliers] = useState<Record<number, string>>({})
  const [settlementNote, setSettlementNote] = useState('')
  const [ignoreDialog, setIgnoreDialog] = useState<IgnoreDialogState | null>(null)

  const loadData = useCallback(async (range?: ProfitLoadRange) => {
    setError(null)
    try {
      const profitSettings = await api.getProfitSettings()
      setSettings(profitSettings)
		setRatio((current) => current === '' ? String(profitSettings.default_settlement_ratio_ppm / PPM) : current)
      if (!profitSettings.enabled) {
        setLoading(false)
        return
      }
      const effectiveStartDate = range?.startDate ?? startDate
      const effectiveEndDate = range?.endDate ?? endDate
      const ratioPPM = Math.max(1, Math.round((Number(ratio) || profitSettings.default_settlement_ratio_ppm / PPM) * PPM))
      const [dashboardResult, groupResult, pendingResult, settlementResult] = await Promise.all([
        api.getProfitDashboard({ startDate: effectiveStartDate, endDate: effectiveEndDate, ratioPPM }),
        api.listProfitGroups(),
        api.listProfitPendingAccounts(),
        api.listProfitSettlements(),
      ])
      setDashboard(dashboardResult)
      setGroups(groupResult.groups)
      setPending(pendingResult.accounts)
      setSettlements(settlementResult.settlements)
      setGroupMultipliers(Object.fromEntries(groupResult.groups.map((group) => [group.group_id, String(group.multiplier_ppm / PPM)])))
      setPendingSelections((current) => {
        const next = { ...current }
        for (const account of pendingResult.accounts) {
          if (!next[account.account_id] && account.operational_groups[0]) {
            next[account.account_id] = String(account.operational_groups[0].id)
          }
        }
        return next
      })
    } catch (loadError) {
      setError(getErrorMessage(loadError))
    } finally {
      setLoading(false)
    }
  }, [endDate, ratio, startDate])

	useEffect(() => { void loadData() }, [])
  const runBusy = async (key: string, action: () => Promise<void>) => {
    setBusy(key)
    try {
      await action()
    } catch (actionError) {
      showToast(getErrorMessage(actionError), 'error')
    } finally {
      setBusy('')
    }
  }

  const applyPreset = (preset: 'week' | 'month' | '30d') => {
    let nextStartDate = startOfWeek(today)
    if (preset === 'month') nextStartDate = `${today.slice(0, 7)}-01`
    if (preset === '30d') nextStartDate = addDays(today, -29)
    const nextEndDate = addDays(today, 1)
    setStartDate(nextStartDate)
    setEndDate(nextEndDate)
    void loadData({ startDate: nextStartDate, endDate: nextEndDate })
  }

  const refreshLedger = () => runBusy('ledger', async () => {
    let processed = 0
    let targetHighWaterID: number | null = null
    for (;;) {
      const result = await api.refreshProfitLedger(100)
      targetHighWaterID ??= result.high_water_id
      processed += result.processed_logs
      const remainingToTarget = Math.max(0, targetHighWaterID - result.checkpoint_id)
      setDashboard((current) => current ? {
        ...current,
        ledger: {
          ...result,
          high_water_id: targetHighWaterID!,
          remaining_logs: remainingToTarget,
          caught_up: remainingToTarget === 0,
        },
      } : current)
      if (remainingToTarget === 0) break
      if (result.processed_logs === 0) throw new Error(`聚合没有取得进展，仍有 ${remainingToTarget} 条日志待处理`)
      await new Promise((resolve) => window.setTimeout(resolve, 200))
    }
    showToast(`已追平到本次点击时的日志截止点，共处理 ${processed} 条`, 'success')
    await loadData()
  })

  const assignGroup = (account: ProfitPendingAccount) => runBusy(`account-${account.account_id}`, async () => {
    const groupID = Number(pendingSelections[account.account_id])
    if (!groupID) throw new Error('请选择结算分组')
    await api.assignProfitSettlementGroup(account.account_id, groupID)
    showToast('已回填历史待确认用量，并设为该账号未来默认结算分组', 'success')
    await loadData()
  })

  const submitIgnore = () => {
    if (!ignoreDialog) return
    if (ignoreDialog.purge && ignoreDialog.stage === 'options') {
      setIgnoreDialog((current) => current ? { ...current, stage: 'confirm-purge', confirmation: '' } : current)
      return
    }
    const { account, purge, confirmation } = ignoreDialog
    if (purge && confirmation.trim() !== PURGE_CONFIRMATION_TEXT) return
    void runBusy(`ignore-${account.account_id}`, async () => {
      await api.ignoreProfitPendingAccount(account.account_id, {
        purge,
        confirm: purge ? PURGE_PROFIT_ACCOUNT_CONFIRM : undefined,
      })
      setIgnoreDialog(null)
      showToast(purge ? '账号及未结算关联数据已彻底删除' : '已忽略该账号，后续不再出现在待确认列表', 'success')
      await loadData()
    })
  }

  const saveGroup = (group: ProfitGroupSetting) => runBusy(`group-${group.group_id}`, async () => {
    const value = Number(groupMultipliers[group.group_id])
    if (!Number.isFinite(value) || value <= 0) throw new Error('倍率必须大于 0')
    await api.updateProfitGroup(group.group_id, Math.round(value * PPM))
    showToast(`已保存 ${group.group_name} 的利润倍率`, 'success')
    await loadData()
  })

  const createSettlement = () => runBusy('create-settlement', async () => {
    if (pending.length > 0) throw new Error('仍有待确认账号，请先完成分组确认')
    if (!dashboard?.ledger.caught_up) throw new Error('日账本尚未追平，请先继续聚合')
    const ratioPPM = Math.max(1, Math.round((Number(ratio) || 1) * PPM))
    const detail = await api.createProfitSettlement({
      start_date: startDate,
      end_date: endDate,
      settlement_ratio_ppm: ratioPPM,
      notes: settlementNote,
    })
    showToast(`已创建结算草稿 ${detail.run.id}`, 'success')
    setActiveTab('settlements')
    await loadData()
  })

  const confirmSettlement = (run: ProfitSettlementRun) => runBusy(`confirm-${run.id}`, async () => {
    if (!window.confirm(`确认结算 ${run.start_date} 至 ${run.end_date}？确认后来源账本将被锁定。`)) return
    await api.confirmProfitSettlement(run.id)
    showToast('结算已确认并锁定来源账本', 'success')
    await loadData()
  })

  const reviseSettlement = (run: ProfitSettlementRun) => runBusy(`revise-${run.id}`, async () => {
    const ratioPPM = Math.max(1, Math.round((Number(ratio) || run.settlement_ratio_ppm / PPM) * PPM))
    await api.reviseProfitSettlement(run.id, { settlement_ratio_ppm: ratioPPM, notes: `修订自 ${run.id}` })
    showToast('已创建修订草稿，原确认记录保持可审计', 'success')
    await loadData()
  })

  const exportSettlement = (run: ProfitSettlementRun) => runBusy(`export-${run.id}`, async () => {
    const blob = await api.exportProfitSettlement(run.id)
    downloadBlob(blob, `利润结算-${run.start_date}-${run.end_date}-R${run.revision_no}.csv`)
  })

  const dimensionRows = useMemo(() => dashboard?.[dimension] ?? [], [dashboard, dimension])
  const allGroupOptions = useMemo(() => groups.filter((group) => !group.historical).map((group) => ({ label: group.group_name, value: String(group.group_id) })), [groups])

  if (loading && !settings) return <StateShell variant="page" loading>{null}</StateShell>
  if (error && !settings) return <StateShell variant="page" error={error} onRetry={() => void loadData()}>{null}</StateShell>

  if (settings && !settings.enabled) {
    return (
      <div>
        <PageHeader title="利润结算中心" description="按官方成本、内部人民币结算比例和分组倍率自动生成可审核的利润账本。" />
        <Card className="mx-auto mt-16 max-w-2xl">
          <CardContent className="flex flex-col items-center py-12 text-center">
            <CircleDollarSign className="size-12 text-muted-foreground" />
            <h3 className="mt-5 text-lg font-semibold">利润结算中心当前未启用</h3>
            <p className="mt-2 max-w-lg text-sm leading-relaxed text-muted-foreground">此功能默认关闭。启用后才会显示侧栏菜单；已有美元成本数据不会被改变。</p>
            <Button asChild className="mt-6"><Link to="/settings"><Settings2 className="size-4" />前往系统设置启用</Link></Button>
          </CardContent>
        </Card>
      </div>
    )
  }

  const tabs: Array<{ id: ProfitTab; label: string; icon: typeof Calculator; count?: number }> = [
    { id: 'dashboard', label: '利润看板', icon: Calculator },
    { id: 'pending', label: '待确认账号', icon: Users, count: pending.length },
    { id: 'groups', label: '分组倍率', icon: Settings2 },
    { id: 'settlements', label: '结算记录', icon: FileClock, count: settlements.filter((run) => run.status === 'draft').length },
  ]

  return (
    <div>
      <PageHeader
        title="利润结算中心"
        description="美元源成本保持不变；人民币结算比例和分组倍率可调整，正式确认后通过修订留痕。"
        actions={<Button variant="outline" onClick={() => void loadData()} disabled={Boolean(busy)}><RefreshCw className="size-4" />刷新</Button>}
      />

      <div className="mb-5 flex flex-wrap gap-2">
        {tabs.map((tab) => {
          const Icon = tab.icon
          return <Button key={tab.id} variant={activeTab === tab.id ? 'default' : 'outline'} onClick={() => setActiveTab(tab.id)}>
            <Icon className="size-4" />{tab.label}{tab.count ? <Badge variant="secondary">{tab.count}</Badge> : null}
          </Button>
        })}
      </div>

      {activeTab === 'dashboard' && dashboard ? (
        <div className="space-y-5">
          <Card>
            <CardContent className="grid gap-3 p-4 lg:grid-cols-[1fr_1fr_160px_auto]">
              <label className="space-y-1.5 text-sm font-medium">开始日期（北京时间）<Input type="date" value={startDate} onChange={(event) => setStartDate(event.target.value)} /></label>
              <label className="space-y-1.5 text-sm font-medium">结束日期（不含）<Input type="date" value={endDate} onChange={(event) => setEndDate(event.target.value)} /></label>
              <label className="space-y-1.5 text-sm font-medium">人民币结算比例<Input type="number" min="0.000001" step="0.01" value={ratio} onChange={(event) => setRatio(event.target.value)} /></label>
              <div className="flex flex-wrap items-end gap-2">
                <Button variant="outline" onClick={() => applyPreset('week')}>本周</Button>
                <Button variant="outline" onClick={() => applyPreset('month')}>本月</Button>
                <Button variant="outline" onClick={() => applyPreset('30d')}>30 天</Button>
                <Button onClick={() => void loadData()}>计算</Button>
              </div>
            </CardContent>
          </Card>

          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <MetricCard title="官方成本" value={formatUSD(dashboard.overall.official_cost_usd_micros)} hint="源账本 USD" />
            <MetricCard title="人民币结算成本" value={formatCNY(dashboard.overall.settlement_cost_cny_micros)} hint={`比例 ${dashboard.settlement_ratio_ppm / PPM}:1`} />
            <MetricCard title="结算收入" value={formatCNY(dashboard.overall.revenue_cny_micros)} hint="按各分组倍率计算" />
            <MetricCard title="预计利润" value={formatCNY(dashboard.overall.profit_cny_micros)} hint={dashboard.overall.margin === null ? '利润率 N/A' : `利润率 ${(dashboard.overall.margin * 100).toFixed(2)}%`} positive={dashboard.overall.profit_cny_micros >= 0} />
          </div>

          <Card className={cn(!dashboard.ledger.caught_up && 'border-amber-500/40')}>
            <CardContent className="flex flex-col gap-3 p-4 sm:flex-row sm:items-center sm:justify-between">
              <div className="flex items-start gap-3">
                {dashboard.ledger.caught_up ? <CheckCircle2 className="mt-0.5 size-5 text-emerald-500" /> : <TriangleAlert className="mt-0.5 size-5 text-amber-500" />}
                <div><div className="font-semibold">{dashboard.ledger.caught_up ? '日账本已追平' : `还有 ${formatNumber(dashboard.ledger.remaining_logs)} 条日志待聚合`}</div><div className="mt-1 text-xs text-muted-foreground">检查点 {dashboard.ledger.checkpoint_id} / {dashboard.ledger.high_water_id}</div></div>
              </div>
              <Button variant={dashboard.ledger.caught_up ? 'outline' : 'default'} disabled={busy === 'ledger'} onClick={refreshLedger}>
                {busy === 'ledger' ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}{busy === 'ledger' ? `正在追平（剩余 ${formatNumber(dashboard.ledger.remaining_logs)}）` : dashboard.ledger.caught_up ? '检查并追平新日志' : '开始追平日志'}
              </Button>
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="flex-row items-center justify-between"><CardTitle>多维度审计</CardTitle><Select value={dimension} onValueChange={(value) => setDimension(value as DimensionKey)} options={[{ label: '结算分组', value: 'groups' }, { label: 'API Key', value: 'api_keys' }, { label: '上游账号', value: 'accounts' }, { label: '模型', value: 'models' }]} /></CardHeader>
            <DimensionTable rows={dimensionRows} />
          </Card>

          <Card>
            <CardHeader><CardTitle>创建结算草稿</CardTitle></CardHeader>
            <CardContent className="flex flex-col gap-3 sm:flex-row sm:items-end">
              <label className="min-w-0 flex-1 space-y-1.5 text-sm font-medium">备注<Input value={settlementNote} onChange={(event) => setSettlementNote(event.target.value)} placeholder="例如：2026 年 8 月第一周内部结算" /></label>
              <Button disabled={busy === 'create-settlement' || !dashboard.ledger.caught_up || pending.length > 0} onClick={createSettlement}>
                {busy === 'create-settlement' ? <Loader2 className="size-4 animate-spin" /> : <FileClock className="size-4" />}生成可审核草稿
              </Button>
            </CardContent>
          </Card>
        </div>
      ) : null}

      {activeTab === 'pending' ? (
        <Card>
          <CardHeader><CardTitle>待确认账号</CardTitle><p className="text-sm text-muted-foreground">确认时会默认带出账号原有业务分组；已删除账号也可以忽略，或在二次确认后彻底清理未结算关联数据。</p></CardHeader>
          <CardContent className="space-y-3">
            {pending.length === 0 ? <EmptyState text="当前没有待确认账号" /> : pending.map((account) => {
              const ownOptions = account.operational_groups.map((group) => ({ label: `${group.name}（原有分组）`, value: String(group.id) }))
              const ownIDs = new Set(ownOptions.map((option) => option.value))
              const options = [...ownOptions, ...allGroupOptions.filter((option) => !ownIDs.has(option.value))]
              return <div key={account.account_id} className="grid gap-3 rounded-xl border border-border p-4 lg:grid-cols-[1fr_220px_auto] lg:items-center">
                <div><div className="flex flex-wrap items-center gap-2 font-semibold">{account.account_name || `账号 #${account.account_id}`}{account.deleted ? <Badge variant="destructive">已删除</Badge> : null}</div><div className="mt-1 text-xs text-muted-foreground">{account.first_date} 至 {account.last_date} · {formatNumber(account.pending_requests)} 次请求 · {formatUSD(account.official_cost_usd_micros)}</div></div>
                <Select value={pendingSelections[account.account_id] ?? ''} onValueChange={(value) => setPendingSelections((current) => ({ ...current, [account.account_id]: value }))} options={options} placeholder="选择结算分组" />
                <div className="flex flex-wrap gap-2 lg:justify-end">
                  <Button disabled={busy === `account-${account.account_id}`} onClick={() => assignGroup(account)}>{busy === `account-${account.account_id}` ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}确认并回填</Button>
                  {account.deleted ? <Button variant="outline" disabled={Boolean(busy)} onClick={() => setIgnoreDialog({ account, purge: false, stage: 'options', confirmation: '' })}><EyeOff className="size-4" />忽略</Button> : null}
                </div>
              </div>
            })}
          </CardContent>
        </Card>
      ) : null}

      {activeTab === 'groups' ? (
        <Card>
          <CardHeader><CardTitle>分组利润倍率</CardTitle><p className="text-sm text-muted-foreground">倍率作用于人民币结算成本。例：1.20 表示按成本的 120% 结算，利润为 20%。</p></CardHeader>
          <CardContent className="space-y-3">
            {groups.map((group) => <div key={group.group_id} className="grid gap-3 rounded-xl border border-border p-4 sm:grid-cols-[1fr_160px_auto] sm:items-center">
              <div><div className="flex items-center gap-2 font-semibold">{group.group_name || `历史分组 #${group.group_id}`}{group.historical ? <Badge variant="secondary">历史分组</Badge> : null}</div><div className="mt-1 text-xs text-muted-foreground">已绑定 {group.assigned_count} 个结算账号</div></div>
              <Input type="number" min="0.000001" step="0.01" value={groupMultipliers[group.group_id] ?? '1'} onChange={(event) => setGroupMultipliers((current) => ({ ...current, [group.group_id]: event.target.value }))} />
              <Button variant="outline" disabled={busy === `group-${group.group_id}`} onClick={() => saveGroup(group)}>{busy === `group-${group.group_id}` ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}保存倍率</Button>
            </div>)}
          </CardContent>
        </Card>
      ) : null}

      {activeTab === 'settlements' ? (
        <Card>
          <CardHeader><CardTitle>结算与修订记录</CardTitle><p className="text-sm text-muted-foreground">草稿可重新计算；确认后锁定来源清单。后续比例或倍率调整通过新修订保留完整审计链。</p></CardHeader>
          <CardContent className="space-y-3">
            {settlements.length === 0 ? <EmptyState text="尚未创建结算记录" /> : settlements.map((run) => <div key={run.id} className="grid gap-3 rounded-xl border border-border p-4 xl:grid-cols-[1.4fr_1fr_auto] xl:items-center">
              <div><div className="flex flex-wrap items-center gap-2 font-semibold">{run.start_date} 至 {run.end_date}<StatusBadge status={run.status} /> <Badge variant="outline">R{run.revision_no}</Badge></div><div className="mt-1 break-all text-xs text-muted-foreground">{run.id} · 来源 {run.source_manifest_hash.slice(0, 16)}…</div></div>
              <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-sm"><span className="text-muted-foreground">收入</span><strong className="text-right text-emerald-600">{formatCNY(run.revenue_cny_micros)}</strong><span className="text-muted-foreground">利润</span><strong className="text-right">{formatCNY(run.profit_cny_micros)}</strong></div>
              <div className="flex flex-wrap gap-2">
                {run.status === 'draft' ? <Button disabled={busy === `confirm-${run.id}`} onClick={() => confirmSettlement(run)}>{busy === `confirm-${run.id}` ? <Loader2 className="size-4 animate-spin" /> : <CheckCircle2 className="size-4" />}确认</Button> : null}
                {run.status === 'confirmed' ? <Button variant="outline" disabled={busy === `revise-${run.id}`} onClick={() => reviseSettlement(run)}><FileClock className="size-4" />创建修订</Button> : null}
                <Button variant="outline" disabled={busy === `export-${run.id}`} onClick={() => exportSettlement(run)}><Download className="size-4" />CSV</Button>
              </div>
            </div>)}
          </CardContent>
        </Card>
      ) : null}

      <Dialog open={Boolean(ignoreDialog)} onOpenChange={(open) => { if (!open && !busy.startsWith('ignore-')) setIgnoreDialog(null) }}>
        <DialogContent className="sm:max-w-lg" showCloseButton={!busy.startsWith('ignore-')}>
          {ignoreDialog?.stage === 'options' ? <>
            <DialogHeader>
              <DialogTitle>忽略已删除账号</DialogTitle>
              <DialogDescription>
                {ignoreDialog.account.account_name || `账号 #${ignoreDialog.account.account_id}`} 将不再出现在利润中心待确认列表。
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              <div className="rounded-lg border border-border bg-muted/30 p-3 text-sm text-muted-foreground">
                仅忽略会保留账号、历史用量和一个轻量忽略标记，并从利润看板及后续结算来源中排除；不会影响 API Key 已累计额度。
              </div>
              <label className="flex cursor-pointer items-start gap-3 rounded-lg border border-red-500/30 p-3">
                <input
                  type="checkbox"
                  className="mt-1 size-4 accent-red-600"
                  checked={ignoreDialog.purge}
                  onChange={(event) => setIgnoreDialog((current) => current ? { ...current, purge: event.target.checked } : current)}
                />
                <span>
                  <span className="block font-semibold text-red-600">同时彻底删除账号及关联数据</span>
                  <span className="mt-1 block text-xs leading-relaxed text-muted-foreground">清理未结算利润账本、用量日志、账号级统计和审查事件。API Key 已累计总额度不会回退；存在结算草稿或已确认结算时会拒绝删除。</span>
                </span>
              </label>
            </div>
            <DialogFooter>
              <Button variant="outline" disabled={busy.startsWith('ignore-')} onClick={() => setIgnoreDialog(null)}>取消</Button>
              <Button variant={ignoreDialog.purge ? 'destructive' : 'default'} disabled={busy.startsWith('ignore-')} onClick={submitIgnore}>
                {ignoreDialog.purge ? <Trash2 className="size-4" /> : <EyeOff className="size-4" />}{ignoreDialog.purge ? '继续二次确认' : '确认忽略'}
              </Button>
            </DialogFooter>
          </> : null}

          {ignoreDialog?.stage === 'confirm-purge' ? <>
            <DialogHeader>
              <DialogTitle className="text-red-600">二次确认：永久删除</DialogTitle>
              <DialogDescription>
                此操作不可恢复。账号 #{ignoreDialog.account.account_id} 及未结算关联数据将被分批清理。
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-2">
              <label className="text-sm font-medium">请输入“{PURGE_CONFIRMATION_TEXT}”以继续</label>
              <Input autoFocus value={ignoreDialog.confirmation} onChange={(event) => setIgnoreDialog((current) => current ? { ...current, confirmation: event.target.value } : current)} />
            </div>
            <DialogFooter>
              <Button variant="outline" disabled={busy.startsWith('ignore-')} onClick={() => setIgnoreDialog((current) => current ? { ...current, stage: 'options', confirmation: '' } : current)}>返回</Button>
              <Button variant="destructive" disabled={busy.startsWith('ignore-') || ignoreDialog.confirmation.trim() !== PURGE_CONFIRMATION_TEXT} onClick={submitIgnore}>
                {busy === `ignore-${ignoreDialog.account.account_id}` ? <Loader2 className="size-4 animate-spin" /> : <Trash2 className="size-4" />}确认彻底删除
              </Button>
            </DialogFooter>
          </> : null}
        </DialogContent>
      </Dialog>
    </div>
  )
}

function MetricCard({ title, value, hint, positive }: { title: string; value: string; hint: string; positive?: boolean }) {
  return <Card><CardContent className="p-5"><div className="text-sm font-medium text-muted-foreground">{title}</div><div className={cn('mt-2 text-2xl font-bold tabular-nums', positive === true && 'text-emerald-600', positive === false && 'text-red-500')}>{value}</div><div className="mt-1 text-xs text-muted-foreground">{hint}</div></CardContent></Card>
}

function DimensionTable({ rows }: { rows: ProfitDashboardDimension[] }) {
  return <div className="overflow-x-auto"><Table><TableHeader><TableRow><TableHead>名称</TableHead><TableHead className="text-right">请求</TableHead><TableHead className="text-right">Token</TableHead><TableHead className="text-right">官方成本</TableHead><TableHead className="text-right">人民币成本</TableHead><TableHead className="text-right">收入</TableHead><TableHead className="text-right">利润</TableHead></TableRow></TableHeader><TableBody>{rows.length === 0 ? <TableRow><TableCell colSpan={7} className="py-10 text-center text-muted-foreground">该范围暂无数据</TableCell></TableRow> : rows.map((row) => <TableRow key={row.id}><TableCell><div className="flex items-center gap-2 font-medium">{row.name || `#${row.id}`}{row.deleted ? <Badge variant="destructive">已删除</Badge> : null}{row.pending ? <Badge variant="secondary">待确认</Badge> : null}</div>{row.multiplier_ppm ? <div className="mt-1 text-xs text-muted-foreground">倍率 {(row.multiplier_ppm / PPM).toFixed(4)}</div> : null}</TableCell><TableCell className="text-right tabular-nums">{formatNumber(row.request_count)}</TableCell><TableCell className="text-right tabular-nums">{formatNumber(row.total_tokens)}</TableCell><TableCell className="text-right tabular-nums">{formatUSD(row.official_cost_usd_micros)}</TableCell><TableCell className="text-right tabular-nums">{formatCNY(row.settlement_cost_cny_micros)}</TableCell><TableCell className="text-right font-semibold tabular-nums text-emerald-600">{formatCNY(row.revenue_cny_micros)}</TableCell><TableCell className="text-right font-semibold tabular-nums">{formatCNY(row.profit_cny_micros)}</TableCell></TableRow>)}</TableBody></Table></div>
}

function EmptyState({ text }: { text: string }) {
  return <div className="flex flex-col items-center py-12 text-center text-sm text-muted-foreground"><CircleDollarSign className="mb-3 size-8 opacity-50" />{text}</div>
}

function StatusBadge({ status }: { status: string }) {
  if (status === 'confirmed') return <Badge className="bg-emerald-600">已确认</Badge>
  if (status === 'superseded') return <Badge variant="secondary">已被修订</Badge>
  return <Badge variant="outline">草稿</Badge>
}
