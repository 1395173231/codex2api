import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  ArrowRightLeft,
  Calculator,
  CheckCircle2,
  CircleDollarSign,
  Download,
  FileClock,
  EyeOff,
  KeyRound,
  ListChecks,
  Loader2,
  RefreshCw,
  Save,
  Settings2,
  Trash2,
  TriangleAlert,
  Users,
  WalletCards,
} from 'lucide-react'
import { api } from '../api'
import type {
  ProfitAccountEconomicSetting,
  ProfitAPIKeyAssignment,
  ProfitDashboardDimension,
  ProfitDashboardResponse,
  ProfitGroupSetting,
  ProfitPairRateSetting,
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
const DIMENSION_PAGE_SIZE = 50
type ProfitTab = 'dashboard' | 'pending' | 'keys' | 'rates' | 'economics' | 'settlements'
type DimensionKey = 'groups' | 'api_keys' | 'accounts' | 'models'
type DimensionAPIKey = 'group' | 'api_key' | 'account' | 'model'
type ProfitLoadRange = { startDate: string; endDate: string }
type PendingActionDialogState =
  | {
      action: 'assign'
      accounts: ProfitPendingAccount[]
      groupID: string
    }
  | {
      action: 'ignore'
      accounts: ProfitPendingAccount[]
      purge: boolean
      stage: 'options' | 'confirm-purge'
      confirmation: string
    }

type PendingActionProgress = {
  completed: number
  total: number
}

const PURGE_PROFIT_ACCOUNT_CONFIRM = 'PURGE-PROFIT-ACCOUNT-DATA'
const PURGE_CONFIRMATION_TEXT = '永久删除'
const dimensionAPIKeys: Record<DimensionKey, DimensionAPIKey> = {
  groups: 'group',
  api_keys: 'api_key',
  accounts: 'account',
  models: 'model',
}

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
  const [apiKeyAssignments, setAPIKeyAssignments] = useState<ProfitAPIKeyAssignment[]>([])
  const [pairRates, setPairRates] = useState<ProfitPairRateSetting[]>([])
  const [accountEconomics, setAccountEconomics] = useState<ProfitAccountEconomicSetting[]>([])
  const [activeTab, setActiveTab] = useState<ProfitTab>('dashboard')
  const [dimension, setDimension] = useState<DimensionKey>('groups')
  const [dimensionRows, setDimensionRows] = useState<ProfitDashboardDimension[]>([])
  const [dimensionPage, setDimensionPage] = useState(1)
  const [dimensionHasMore, setDimensionHasMore] = useState(false)
  const [dimensionLoading, setDimensionLoading] = useState(false)
  const [startDate, setStartDate] = useState(startOfWeek(today))
  const [endDate, setEndDate] = useState(addDays(today, 1))
  const [ratio, setRatio] = useState('')
  const [economicsMonth, setEconomicsMonth] = useState(`${today.slice(0, 7)}-01`)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [pendingSelections, setPendingSelections] = useState<Record<number, string>>({})
  const [selectedPendingIDs, setSelectedPendingIDs] = useState<number[]>([])
  const [keySelections, setKeySelections] = useState<Record<number, string>>({})
  const [keyApplyHistory, setKeyApplyHistory] = useState<Record<number, boolean>>({})
  const [pairConsumerGroupID, setPairConsumerGroupID] = useState('')
  const [pairOwnerGroupID, setPairOwnerGroupID] = useState('')
  const [pairRate, setPairRate] = useState('0.1')
  const [pairEffectiveDate, setPairEffectiveDate] = useState(today)
  const [accountEconomicInputs, setAccountEconomicInputs] = useState<Record<number, { cost: string; capacity: string }>>({})
  const [settlementNote, setSettlementNote] = useState('')
  const [pendingActionDialog, setPendingActionDialog] = useState<PendingActionDialogState | null>(null)
  const [pendingActionProgress, setPendingActionProgress] = useState<PendingActionProgress | null>(null)

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
      const [dashboardResult, dimensionResult, groupResult, pendingResult, settlementResult, keyResult, rateResult, economicResult] = await Promise.all([
        api.getProfitDashboard({ startDate: effectiveStartDate, endDate: effectiveEndDate, ratioPPM }),
        api.getProfitDashboardDimension(dimensionAPIKeys[dimension], {
          startDate: effectiveStartDate,
          endDate: effectiveEndDate,
          page: 1,
          pageSize: DIMENSION_PAGE_SIZE,
        }),
        api.listProfitGroups(),
        api.listProfitPendingAccounts(),
        api.listProfitSettlements(),
        api.listProfitAPIKeyAssignments(),
        api.listProfitPairRates(),
        api.listProfitAccountEconomics(economicsMonth),
      ])
      setDashboard(dashboardResult)
      setDimensionRows(dimensionResult.items)
      setDimensionPage(1)
      setDimensionHasMore(dimensionResult.items.length === DIMENSION_PAGE_SIZE)
      setGroups(groupResult.groups)
      setPending(pendingResult.accounts)
      const pendingIDs = new Set(pendingResult.accounts.map((account) => account.account_id))
      setSelectedPendingIDs((current) => current.filter((accountID) => pendingIDs.has(accountID)))
      setSettlements(settlementResult.settlements)
      setAPIKeyAssignments(keyResult.api_keys)
      setPairRates(rateResult.rates)
      setAccountEconomics(economicResult.accounts)
      setKeySelections(Object.fromEntries(keyResult.api_keys.map((item) => [
        item.api_key_id,
        String(item.consumer_group_id || item.suggested_group_id || ''),
      ])))
      setAccountEconomicInputs(Object.fromEntries(economicResult.accounts.map((item) => [item.account_id, {
        cost: String(item.monthly_fixed_cost_usd_micros / PPM),
        capacity: String(item.monthly_capacity_usd_micros / PPM),
      }])))
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
  }, [dimension, economicsMonth, endDate, ratio, startDate])

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

  const loadDimensionPage = async (nextDimension: DimensionKey, page: number, append: boolean) => {
    setDimensionLoading(true)
    try {
      const result = await api.getProfitDashboardDimension(dimensionAPIKeys[nextDimension], {
        startDate,
        endDate,
        page,
        pageSize: DIMENSION_PAGE_SIZE,
      })
      setDimensionRows((current) => append ? [...current, ...result.items] : result.items)
      setDimensionPage(page)
      setDimensionHasMore(result.items.length === DIMENSION_PAGE_SIZE)
    } catch (dimensionError) {
      showToast(getErrorMessage(dimensionError), 'error')
    } finally {
      setDimensionLoading(false)
    }
  }

  const changeDimension = (value: string) => {
    const nextDimension = value as DimensionKey
    setDimension(nextDimension)
    void loadDimensionPage(nextDimension, 1, false)
  }

  const loadEconomics = (month: string) => runBusy('economics-month', async () => {
    const result = await api.listProfitAccountEconomics(month)
    setEconomicsMonth(month)
    setAccountEconomics(result.accounts)
    setAccountEconomicInputs(Object.fromEntries(result.accounts.map((item) => [item.account_id, {
      cost: String(item.monthly_fixed_cost_usd_micros / PPM),
      capacity: String(item.monthly_capacity_usd_micros / PPM),
    }])))
  })

  const saveAPIKeyAssignment = (item: ProfitAPIKeyAssignment) => runBusy(`key-${item.api_key_id}`, async () => {
    const consumerGroupID = Number(keySelections[item.api_key_id])
    if (!consumerGroupID) throw new Error('请选择该 Key 的实际使用方分组')
    const applyHistory = Boolean(keyApplyHistory[item.api_key_id])
    const updated = await api.assignProfitAPIKeyConsumerGroup(item.api_key_id, {
      consumer_group_id: consumerGroupID,
      apply_history: applyHistory,
      reason: applyHistory ? '管理员确认并回填未结算历史' : '管理员确认未来使用方',
    })
    setAPIKeyAssignments((current) => current.map((candidate) => candidate.api_key_id === updated.api_key_id ? updated : candidate))
    setKeyApplyHistory((current) => ({ ...current, [item.api_key_id]: false }))
    showToast(applyHistory ? '已保存使用方，并回填未结算历史' : '已保存 Key 的未来使用方', 'success')
    if (applyHistory) await loadData()
  })

  const savePairRate = () => runBusy('pair-rate', async () => {
    const consumerGroupID = Number(pairConsumerGroupID)
    const ownerGroupID = Number(pairOwnerGroupID)
    const rateValue = Number(pairRate)
    if (!consumerGroupID || !ownerGroupID) throw new Error('请选择使用方和账号所有方')
    if (consumerGroupID === ownerGroupID) throw new Error('同组使用不产生结算，无需设置比例')
    if (!Number.isFinite(rateValue) || rateValue <= 0) throw new Error('结算比例必须大于 0')
    await api.updateProfitPairRate({
      consumer_group_id: consumerGroupID,
      owner_group_id: ownerGroupID,
      rate_ppm: Math.round(rateValue * PPM),
      effective_date: pairEffectiveDate,
      reason: '管理员调整方向结算比例',
    })
    showToast('方向结算比例已保存；历史正式结算不会被直接改写', 'success')
    await loadData()
  })

  const saveAccountEconomic = (item: ProfitAccountEconomicSetting) => runBusy(`economic-${item.account_id}`, async () => {
    const input = accountEconomicInputs[item.account_id]
    const monthlyCost = Number(input?.cost)
    const monthlyCapacity = Number(input?.capacity)
    if (!Number.isFinite(monthlyCost) || monthlyCost < 0) throw new Error('月固定成本不能小于 0')
    if (!Number.isFinite(monthlyCapacity) || monthlyCapacity <= 0) throw new Error('月估算额度必须大于 0')
    const updated = await api.updateProfitAccountEconomic(item.account_id, {
      effective_month: economicsMonth,
      monthly_fixed_cost_usd_micros: Math.round(monthlyCost * PPM),
      monthly_capacity_usd_micros: Math.round(monthlyCapacity * PPM),
      reason: '管理员调整账号月成本与估算额度',
    })
    setAccountEconomics((current) => current.map((candidate) => candidate.account_id === updated.account_id ? updated : candidate))
    showToast(`已保存 ${item.account_name || `账号 #${item.account_id}`} 的月度成本参数`, 'success')
    await loadData()
  })

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
      const result = await api.refreshProfitLedger(20_000)
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

  const openAssignDialog = (accounts: ProfitPendingAccount[]) => {
    if (accounts.length === 0) return
    const selectedGroups = new Set(accounts.map((account) => pendingSelections[account.account_id]).filter(Boolean))
    setPendingActionDialog({
      action: 'assign',
      accounts,
      groupID: selectedGroups.size === 1 ? Array.from(selectedGroups)[0] : '',
    })
  }

  const openIgnoreDialog = (accounts: ProfitPendingAccount[]) => {
    if (accounts.length === 0) return
    if (accounts.some((account) => !account.deleted)) {
      showToast('忽略或彻底删除仅适用于已删除、已进入回收站的账号', 'error')
      return
    }
    setPendingActionDialog({ action: 'ignore', accounts, purge: false, stage: 'options', confirmation: '' })
  }

  const submitPendingAction = () => {
    if (!pendingActionDialog) return
    if (pendingActionDialog.action === 'ignore' && pendingActionDialog.purge && pendingActionDialog.stage === 'options') {
      setPendingActionDialog((current) => current?.action === 'ignore'
        ? { ...current, stage: 'confirm-purge', confirmation: '' }
        : current)
      return
    }
    if (pendingActionDialog.action === 'assign' && !Number(pendingActionDialog.groupID)) return
    if (pendingActionDialog.action === 'ignore'
      && pendingActionDialog.purge
      && pendingActionDialog.confirmation.trim() !== PURGE_CONFIRMATION_TEXT) return

    const action = pendingActionDialog
    void runBusy('pending-action', async () => {
      const failures: Array<{ account: ProfitPendingAccount; error: string }> = []
      const succeededIDs = new Set<number>()
      setPendingActionProgress({ completed: 0, total: action.accounts.length })
      for (const [index, account] of action.accounts.entries()) {
        try {
          if (action.action === 'assign') {
            await api.assignProfitSettlementGroup(account.account_id, Number(action.groupID))
          } else {
            await api.ignoreProfitPendingAccount(account.account_id, {
              purge: action.purge,
              confirm: action.purge ? PURGE_PROFIT_ACCOUNT_CONFIRM : undefined,
            })
          }
          succeededIDs.add(account.account_id)
        } catch (accountError) {
          failures.push({ account, error: getErrorMessage(accountError) })
        } finally {
          setPendingActionProgress({ completed: index + 1, total: action.accounts.length })
        }
      }

      setSelectedPendingIDs((current) => current.filter((accountID) => !succeededIDs.has(accountID)))
      setPendingActionDialog(null)
      setPendingActionProgress(null)
      await loadData()

      if (failures.length > 0) {
        const summary = failures.slice(0, 3)
          .map(({ account, error: failureError }) => `${account.account_name || `#${account.account_id}`}：${failureError}`)
          .join('；')
        throw new Error(`已完成 ${action.accounts.length - failures.length}/${action.accounts.length} 个账号，失败 ${failures.length} 个。${summary}`)
      }

      if (action.action === 'assign') {
        showToast(`已为 ${action.accounts.length} 个账号确认分组并回填历史用量`, 'success')
      } else {
        showToast(action.purge
          ? `已彻底删除 ${action.accounts.length} 个账号及未结算关联数据`
          : `已忽略 ${action.accounts.length} 个账号`, 'success')
      }
    })
  }

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

  const allGroupOptions = useMemo(() => groups.filter((group) => !group.historical).map((group) => ({ label: group.group_name, value: String(group.group_id) })), [groups])
  const fixedCostSummary = useMemo(() => (dashboard?.account_roi ?? []).reduce((summary, item) => ({
    allocatedUSD: summary.allocatedUSD + item.allocated_in_range_usd_micros,
    allocatedCNY: summary.allocatedCNY + item.allocated_in_range_cny_micros,
    remainingUSD: summary.remainingUSD + item.remaining_fixed_cost_usd_micros,
  }), { allocatedUSD: 0, allocatedCNY: 0, remainingUSD: 0 }), [dashboard])
  const selectedPendingIDSet = useMemo(() => new Set(selectedPendingIDs), [selectedPendingIDs])
  const selectedPendingAccounts = useMemo(
    () => pending.filter((account) => selectedPendingIDSet.has(account.account_id)),
    [pending, selectedPendingIDSet],
  )
  const allPendingSelected = pending.length > 0 && selectedPendingAccounts.length === pending.length
  const selectedAccountsIncludeActive = selectedPendingAccounts.some((account) => !account.deleted)
  const pendingDialogGroupOptions = useMemo(() => {
    if (!pendingActionDialog) return allGroupOptions
    const originalGroups = new Map<number, string>()
    for (const account of pendingActionDialog.accounts) {
      for (const group of account.operational_groups) originalGroups.set(group.id, group.name)
    }
    const originalOptions = Array.from(originalGroups, ([id, name]) => ({
      label: `${name}（所选账号原有分组）`,
      value: String(id),
    }))
    const originalIDs = new Set(originalOptions.map((option) => option.value))
    return [...originalOptions, ...allGroupOptions.filter((option) => !originalIDs.has(option.value))]
  }, [allGroupOptions, pendingActionDialog])

  const togglePendingSelection = (accountID: number, checked: boolean) => {
    setSelectedPendingIDs((current) => checked
      ? current.includes(accountID) ? current : [...current, accountID]
      : current.filter((id) => id !== accountID))
  }

  if (loading && !settings) return <StateShell variant="page" loading>{null}</StateShell>
  if (error && !settings) return <StateShell variant="page" error={error} onRetry={() => void loadData()}>{null}</StateShell>

  if (settings && !settings.enabled) {
    return (
      <div>
        <PageHeader title="利润结算中心" description="按使用方与账号所有方生成可审核的双边结算和账号成本回收账本。" />
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
    { id: 'dashboard', label: '结算看板', icon: Calculator },
    { id: 'pending', label: '待确认账号', icon: Users, count: pending.length },
    { id: 'keys', label: 'Key 使用方', icon: KeyRound, count: apiKeyAssignments.filter((item) => item.pending).length },
    { id: 'rates', label: '方向结算比例', icon: ArrowRightLeft },
    { id: 'economics', label: '账号成本', icon: WalletCards },
    { id: 'settlements', label: '结算记录', icon: FileClock, count: settlements.filter((run) => run.status === 'draft').length },
  ]

  return (
    <div>
      <PageHeader
        title="利润结算中心"
        description="自己使用自己的账号不结算；跨分组按使用方 → 账号所有方的方向比例计算人民币应付和应收。"
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
              <label className="space-y-1.5 text-sm font-medium">账号成本折算比例（USD→CNY）<Input type="number" min="0.000001" step="0.01" value={ratio} onChange={(event) => setRatio(event.target.value)} /></label>
              <div className="flex flex-wrap items-end gap-2">
                <Button variant="outline" onClick={() => applyPreset('week')}>本周</Button>
                <Button variant="outline" onClick={() => applyPreset('month')}>本月</Button>
                <Button variant="outline" onClick={() => applyPreset('30d')}>30 天</Button>
                <Button onClick={() => void loadData()}>计算</Button>
              </div>
            </CardContent>
          </Card>

          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
            <MetricCard title="官方用量" value={formatUSD(dashboard.settlement.official_cost_usd_micros)} hint="所选范围内的官方计价金额" />
            <MetricCard title="跨分组占用" value={formatUSD(dashboard.settlement.cross_group_usd_micros)} hint="仅这部分参与双方人民币结算" />
            <MetricCard title="自己账号用量" value={formatUSD(dashboard.settlement.self_usage_usd_micros)} hint="计入账号产能，但应付与应收均为 0" />
            <MetricCard title="总应付" value={formatCNY(dashboard.settlement.payable_cny_micros)} hint="各使用方需要支付的合计" />
            <MetricCard title="总应收" value={formatCNY(dashboard.settlement.receivable_cny_micros)} hint="各账号所有方应收的合计" />
            <MetricCard title="已回收账号成本" value={formatCNY(fixedCostSummary.allocatedCNY)} hint={`${formatUSD(fixedCostSummary.allocatedUSD)} · 成本折算 ${dashboard.settlement_ratio_ppm / PPM}:1`} />
          </div>

          <div className="rounded-xl border border-blue-500/25 bg-blue-500/5 px-4 py-3 text-sm text-muted-foreground">
            全局总应付与总应收应保持一致（当前差额 {formatCNY(dashboard.settlement.global_net_cny_micros)}），这是双边结算守恒校验，不代表平台利润。账号固定成本回收单独计算，尚未回收 {formatUSD(fixedCostSummary.remainingUSD)}。
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
            <CardHeader><CardTitle>分组应收 / 应付汇总</CardTitle><p className="text-sm text-muted-foreground">正数表示净应收，负数表示净应付；自己账号用量不参与双方结算。</p></CardHeader>
            <GroupSettlementTable rows={dashboard.group_settlements ?? []} />
          </Card>

          <Card>
            <CardHeader><CardTitle>跨分组结算明细</CardTitle><p className="text-sm text-muted-foreground">方向为“使用方 → 账号所有方”。例如凡人使用打铁账号 4,000 USD，比例 0.1，则凡人应付、打铁应收均为 ¥400。</p></CardHeader>
            <SettlementMatrixTable rows={dashboard.settlement_matrix ?? []} />
          </Card>

          <Card>
            <CardHeader><CardTitle>账号固定成本回收</CardTitle><p className="text-sm text-muted-foreground">默认月成本 200 USD、月估算额度 10,000 USD，可在“账号成本”菜单按月修改。</p></CardHeader>
            <AccountROITable rows={dashboard.account_roi ?? []} />
          </Card>

          <Card>
            <CardHeader className="flex-row items-center justify-between gap-3"><CardTitle>多维度用量审计</CardTitle><Select value={dimension} onValueChange={changeDimension} dropdownMinWidth={240} options={[{ label: '结算分组', value: 'groups' }, { label: 'API Key', value: 'api_keys' }, { label: '上游账号', value: 'accounts' }, { label: '模型', value: 'models' }]} /></CardHeader>
            {dimensionLoading && dimensionRows.length === 0 ? <StateShell loading>{null}</StateShell> : <DimensionTable rows={dimensionRows} />}
            {dimensionHasMore ? <div className="flex justify-center border-t border-border p-3"><Button variant="outline" disabled={dimensionLoading} onClick={() => void loadDimensionPage(dimension, dimensionPage + 1, true)}>{dimensionLoading ? <Loader2 className="size-4 animate-spin" /> : null}加载更多</Button></div> : null}
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
            {pending.length === 0 ? <EmptyState text="当前没有待确认账号" /> : <>
              <div className={cn(
                'flex flex-col gap-3 rounded-xl border p-3 transition-colors lg:flex-row lg:items-center lg:justify-between',
                selectedPendingAccounts.length > 0 ? 'border-primary/40 bg-primary/5' : 'border-border bg-muted/20',
              )}>
                <div className="flex flex-wrap items-center gap-3">
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={Boolean(busy)}
                    onClick={() => setSelectedPendingIDs(allPendingSelected ? [] : pending.map((account) => account.account_id))}
                  >
                    <ListChecks className="size-4" />{allPendingSelected ? '清空选择' : '全选账号'}
                  </Button>
                  <div className="text-sm">
                    已选择 <strong>{selectedPendingAccounts.length}</strong> / {pending.length} 个账号
                  </div>
                  {selectedAccountsIncludeActive ? <span className="text-xs text-amber-600">当前选择包含未删除账号，只能批量设置分组</span> : null}
                </div>
                <div className="flex flex-wrap gap-2">
                  <Button
                    size="sm"
                    disabled={selectedPendingAccounts.length === 0 || Boolean(busy)}
                    onClick={() => openAssignDialog(selectedPendingAccounts)}
                  >
                    <Save className="size-4" />批量设置分组
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={selectedPendingAccounts.length === 0 || selectedAccountsIncludeActive || Boolean(busy)}
                    title={selectedAccountsIncludeActive ? '忽略或删除仅适用于已删除账号' : undefined}
                    onClick={() => openIgnoreDialog(selectedPendingAccounts)}
                  >
                    <EyeOff className="size-4" />批量忽略 / 删除
                  </Button>
                </div>
              </div>

              {pending.map((account) => {
                const ownOptions = account.operational_groups.map((group) => ({ label: `${group.name}（原有分组）`, value: String(group.id) }))
                const ownIDs = new Set(ownOptions.map((option) => option.value))
                const options = [...ownOptions, ...allGroupOptions.filter((option) => !ownIDs.has(option.value))]
                const selected = selectedPendingIDSet.has(account.account_id)
                return <div key={account.account_id} className={cn(
                  'grid gap-3 rounded-xl border p-4 transition-colors lg:grid-cols-[auto_minmax(0,1fr)_220px_auto] lg:items-center',
                  selected ? 'border-primary/40 bg-primary/5' : 'border-border',
                )}>
                  <input
                    type="checkbox"
                    className="mt-1 size-4 accent-primary lg:mt-0"
                    aria-label={`选择${account.account_name || `账号 #${account.account_id}`}`}
                    checked={selected}
                    disabled={Boolean(busy)}
                    onChange={(event) => togglePendingSelection(account.account_id, event.target.checked)}
                  />
                  <div><div className="flex flex-wrap items-center gap-2 font-semibold">{account.account_name || `账号 #${account.account_id}`}{account.deleted ? <Badge variant="destructive">已删除</Badge> : null}</div><div className="mt-1 text-xs text-muted-foreground">{account.first_date} 至 {account.last_date} · {formatNumber(account.pending_requests)} 次请求 · {formatUSD(account.official_cost_usd_micros)}</div></div>
                  <Select value={pendingSelections[account.account_id] ?? ''} onValueChange={(value) => setPendingSelections((current) => ({ ...current, [account.account_id]: value }))} options={options} placeholder="选择结算分组" dropdownMinWidth={280} />
                  <div className="flex flex-wrap gap-2 lg:justify-end">
                    <Button disabled={Boolean(busy)} onClick={() => openAssignDialog([account])}><Save className="size-4" />确认并回填</Button>
                    {account.deleted ? <Button variant="outline" disabled={Boolean(busy)} onClick={() => openIgnoreDialog([account])}><EyeOff className="size-4" />忽略</Button> : null}
                  </div>
                </div>
              })}
            </>}
          </CardContent>
        </Card>
      ) : null}

      {activeTab === 'keys' ? (
        <Card>
          <CardHeader><CardTitle>API Key 实际使用方</CardTitle><p className="text-sm text-muted-foreground">这里定义“谁在使用这个 Key”。账号允许路由到哪些分组不等于实际使用方；只有明确归属后，才能判断谁占用了谁的账号。</p></CardHeader>
          <CardContent className="space-y-3">
            {apiKeyAssignments.length === 0 ? <EmptyState text="当前没有 API Key" /> : apiKeyAssignments.map((item) => <div key={item.api_key_id} className="grid gap-3 rounded-xl border border-border p-4 xl:grid-cols-[minmax(0,1fr)_240px_220px_auto] xl:items-center">
              <div className="min-w-0"><div className="flex flex-wrap items-center gap-2 font-semibold"><span className="truncate">{item.api_key_name || `Key #${item.api_key_id}`}</span>{item.pending ? <Badge variant="secondary">待确认</Badge> : <Badge variant="outline">已确认</Badge>}</div><div className="mt-1 text-xs text-muted-foreground">当前：{item.consumer_group_name || '未设置'} · 来源：{assignmentSourceLabel(item.assignment_source)}{item.suggested_group_name ? ` · 建议：${item.suggested_group_name}` : ''}</div></div>
              <Select value={keySelections[item.api_key_id] ?? ''} onValueChange={(value) => setKeySelections((current) => ({ ...current, [item.api_key_id]: value }))} options={allGroupOptions} placeholder="选择实际使用方" dropdownMinWidth={280} />
              <label className="flex items-start gap-2 text-sm"><input type="checkbox" className="mt-1 size-4 accent-primary" checked={Boolean(keyApplyHistory[item.api_key_id])} onChange={(event) => setKeyApplyHistory((current) => ({ ...current, [item.api_key_id]: event.target.checked }))} /><span>回填未正式结算历史<span className="block text-xs text-muted-foreground">仅在确认历史归属时勾选</span></span></label>
              <Button variant="outline" disabled={busy === `key-${item.api_key_id}` || !Number(keySelections[item.api_key_id])} onClick={() => saveAPIKeyAssignment(item)}>{busy === `key-${item.api_key_id}` ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}保存</Button>
            </div>)}
          </CardContent>
        </Card>
      ) : null}

      {activeTab === 'rates' ? (
        <div className="space-y-4">
          <Card>
            <CardHeader><CardTitle>设置方向结算比例</CardTitle><p className="text-sm text-muted-foreground">比例按“使用方 → 账号所有方”分别设置，0.1 表示每使用 1 USD 官方额度结算 ¥0.10；同组使用始终为 0。</p></CardHeader>
            <CardContent className="grid gap-3 lg:grid-cols-[1fr_auto_1fr_160px_170px_auto] lg:items-end">
              <label className="space-y-1.5 text-sm font-medium">使用方<Select value={pairConsumerGroupID} onValueChange={setPairConsumerGroupID} options={allGroupOptions} placeholder="选择使用方" dropdownMinWidth={240} /></label>
              <ArrowRightLeft className="mb-2 hidden size-5 text-muted-foreground lg:block" />
              <label className="space-y-1.5 text-sm font-medium">账号所有方<Select value={pairOwnerGroupID} onValueChange={setPairOwnerGroupID} options={allGroupOptions} placeholder="选择账号所有方" dropdownMinWidth={240} /></label>
              <label className="space-y-1.5 text-sm font-medium">人民币比例<Input type="number" min="0.000001" step="0.01" value={pairRate} onChange={(event) => setPairRate(event.target.value)} /></label>
              <label className="space-y-1.5 text-sm font-medium">生效日期<Input type="date" value={pairEffectiveDate} onChange={(event) => setPairEffectiveDate(event.target.value)} /></label>
              <Button disabled={busy === 'pair-rate'} onClick={savePairRate}>{busy === 'pair-rate' ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}保存比例</Button>
            </CardContent>
          </Card>
          <Card>
            <CardHeader><CardTitle>已配置比例版本</CardTitle><p className="text-sm text-muted-foreground">同一方向可按不同日期保留版本；历史已确认结算通过修订处理，不会被静默覆盖。</p></CardHeader>
            <div className="overflow-x-auto"><Table><TableHeader><TableRow><TableHead>方向</TableHead><TableHead className="text-right">比例</TableHead><TableHead>生效日期</TableHead><TableHead>来源</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader><TableBody>{pairRates.length === 0 ? <TableRow><TableCell colSpan={5} className="py-10 text-center text-muted-foreground">尚未设置，未配置方向默认按 0.1 计算</TableCell></TableRow> : pairRates.map((item) => <TableRow key={item.id}><TableCell className="font-medium">{item.consumer_group_name} <ArrowRightLeft className="mx-1 inline size-3.5" /> {item.owner_group_name}</TableCell><TableCell className="text-right tabular-nums">{formatRate(item.rate_ppm)}</TableCell><TableCell>{item.effective_date}</TableCell><TableCell>{item.source === 'manual' ? '手动设置' : item.source}</TableCell><TableCell className="text-right"><Button size="sm" variant="ghost" onClick={() => { setPairConsumerGroupID(String(item.consumer_group_id)); setPairOwnerGroupID(String(item.owner_group_id)); setPairRate(String(item.rate_ppm / PPM)); setPairEffectiveDate(item.effective_date) }}>复制到编辑区</Button></TableCell></TableRow>)}</TableBody></Table></div>
          </Card>
        </div>
      ) : null}

      {activeTab === 'economics' ? (
        <Card>
          <CardHeader className="flex-row items-end justify-between gap-3"><div><CardTitle>账号月固定成本与估算额度</CardTitle><p className="mt-1 text-sm text-muted-foreground">默认月成本 200 USD、月估算额度 10,000 USD。成本回收 = 月成本 × 本次用量 / 月估算额度，并限制不超过尚未回收成本。</p></div><label className="space-y-1.5 text-sm font-medium">查看月份<Input type="month" value={economicsMonth.slice(0, 7)} onChange={(event) => { if (event.target.value) void loadEconomics(`${event.target.value}-01`) }} /></label></CardHeader>
          <CardContent className="space-y-3">
            {accountEconomics.length === 0 ? <EmptyState text="当前没有可配置账号" /> : accountEconomics.map((item) => <div key={item.account_id} className="grid gap-3 rounded-xl border border-border p-4 xl:grid-cols-[minmax(0,1fr)_180px_180px_auto] xl:items-end">
              <div><div className="flex flex-wrap items-center gap-2 font-semibold">{item.account_name || `账号 #${item.account_id}`}{item.account_deleted ? <Badge variant="destructive">已删除</Badge> : null}{item.frozen ? <Badge variant="secondary">本月已结算锁定</Badge> : null}</div><div className="mt-1 text-xs text-muted-foreground">{item.source === 'system_default' ? '系统默认值' : `版本 R${item.revision_no}`} · 生效月 {item.effective_month}</div></div>
              <label className="space-y-1.5 text-sm font-medium">月固定成本（USD）<Input type="number" min="0" step="1" value={accountEconomicInputs[item.account_id]?.cost ?? '200'} disabled={item.frozen} onChange={(event) => setAccountEconomicInputs((current) => ({ ...current, [item.account_id]: { cost: event.target.value, capacity: current[item.account_id]?.capacity ?? '10000' } }))} /></label>
              <label className="space-y-1.5 text-sm font-medium">月估算额度（USD）<Input type="number" min="0.01" step="100" value={accountEconomicInputs[item.account_id]?.capacity ?? '10000'} disabled={item.frozen} onChange={(event) => setAccountEconomicInputs((current) => ({ ...current, [item.account_id]: { cost: current[item.account_id]?.cost ?? '200', capacity: event.target.value } }))} /></label>
              <Button variant="outline" disabled={item.frozen || busy === `economic-${item.account_id}`} onClick={() => saveAccountEconomic(item)}>{busy === `economic-${item.account_id}` ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}{item.frozen ? '已锁定' : '保存'}</Button>
            </div>)}
          </CardContent>
        </Card>
      ) : null}

      {activeTab === 'settlements' ? (
        <Card>
          <CardHeader><CardTitle>结算与修订记录</CardTitle><p className="text-sm text-muted-foreground">草稿生成采用分批短事务；确认后锁定来源清单。后续方向比例或账号成本调整通过新修订保留完整审计链。</p></CardHeader>
          <CardContent className="space-y-3">
            {settlements.length === 0 ? <EmptyState text="尚未创建结算记录" /> : settlements.map((run) => <div key={run.id} className="grid gap-3 rounded-xl border border-border p-4 xl:grid-cols-[1.4fr_1fr_auto] xl:items-center">
              <div><div className="flex flex-wrap items-center gap-2 font-semibold">{run.start_date} 至 {run.end_date}<StatusBadge status={run.status} /> <Badge variant="outline">R{run.revision_no}</Badge></div><div className="mt-1 break-all text-xs text-muted-foreground">{run.id}{run.source_manifest_hash ? ` · 来源 ${run.source_manifest_hash.slice(0, 16)}…` : ''}{run.source_high_water_id ? ` · 截止日志 ${run.source_high_water_id}` : ''}</div>{run.build_error ? <div className="mt-2 text-xs text-red-500">生成失败：{run.build_error}</div> : null}</div>
              <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-sm"><span className="text-muted-foreground">应收</span><strong className="text-right text-emerald-600">{formatCNY(run.receivable_cny_micros)}</strong><span className="text-muted-foreground">应付</span><strong className="text-right text-amber-600">{formatCNY(run.payable_cny_micros)}</strong><span className="text-muted-foreground">成本回收</span><strong className="text-right">{formatUSD(run.fixed_cost_allocated_usd_micros)}</strong></div>
              <div className="flex flex-wrap gap-2">
                {run.status === 'draft' ? <Button disabled={busy === `confirm-${run.id}`} onClick={() => confirmSettlement(run)}>{busy === `confirm-${run.id}` ? <Loader2 className="size-4 animate-spin" /> : <CheckCircle2 className="size-4" />}确认</Button> : null}
                {run.status === 'confirmed' ? <Button variant="outline" disabled={busy === `revise-${run.id}`} onClick={() => reviseSettlement(run)}><FileClock className="size-4" />创建修订</Button> : null}
                <Button variant="outline" disabled={busy === `export-${run.id}`} onClick={() => exportSettlement(run)}><Download className="size-4" />CSV</Button>
              </div>
            </div>)}
          </CardContent>
        </Card>
      ) : null}

      <Dialog open={Boolean(pendingActionDialog)} onOpenChange={(open) => {
        if (!open && busy !== 'pending-action') {
          setPendingActionDialog(null)
          setPendingActionProgress(null)
        }
      }}>
        <DialogContent className="sm:max-w-2xl" showCloseButton={busy !== 'pending-action'}>
          {pendingActionDialog?.action === 'assign' ? <>
            <DialogHeader>
              <DialogTitle>确认设置结算分组</DialogTitle>
              <DialogDescription>
                请核对下面 {pendingActionDialog.accounts.length} 个账号。确认后会回填历史待确认用量，并将所选分组设为这些账号未来的默认结算分组。
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              <label className="block space-y-1.5 text-sm font-medium">
                统一设置为
                <Select
                  value={pendingActionDialog.groupID}
                  onValueChange={(value) => setPendingActionDialog((current) => current?.action === 'assign' ? { ...current, groupID: value } : current)}
                  options={pendingDialogGroupOptions}
                  placeholder="选择结算分组"
                  dropdownMinWidth={320}
                />
              </label>
              <PendingAccountConfirmationList accounts={pendingActionDialog.accounts} />
              <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-muted-foreground">
                这是最终确认步骤。批量操作会逐个账号执行，避免同时写入数据库造成锁竞争；失败账号会单独汇总，不会回滚已成功账号。
              </div>
            </div>
            <DialogFooter>
              <Button variant="outline" disabled={busy === 'pending-action'} onClick={() => setPendingActionDialog(null)}>取消</Button>
              <Button disabled={busy === 'pending-action' || !Number(pendingActionDialog.groupID)} onClick={submitPendingAction}>
                {busy === 'pending-action' ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                {busy === 'pending-action' && pendingActionProgress
                  ? `正在设置 ${pendingActionProgress.completed}/${pendingActionProgress.total}`
                  : `确认并设置 ${pendingActionDialog.accounts.length} 个账号`}
              </Button>
            </DialogFooter>
          </> : null}

          {pendingActionDialog?.action === 'ignore' && pendingActionDialog.stage === 'options' ? <>
            <DialogHeader>
              <DialogTitle>忽略已删除账号</DialogTitle>
              <DialogDescription>
                请核对下面 {pendingActionDialog.accounts.length} 个账号。仅忽略时，它们将不再出现在利润中心待确认列表。
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              <PendingAccountConfirmationList accounts={pendingActionDialog.accounts} />
              <div className="rounded-lg border border-border bg-muted/30 p-3 text-sm text-muted-foreground">
                仅忽略会保留账号、历史用量和一个轻量忽略标记，并从利润看板及后续结算来源中排除；不会影响 API Key 已累计额度。
              </div>
              <label className="flex cursor-pointer items-start gap-3 rounded-lg border border-red-500/30 p-3">
                <input
                  type="checkbox"
                  className="mt-1 size-4 accent-red-600"
                  checked={pendingActionDialog.purge}
                  onChange={(event) => setPendingActionDialog((current) => current?.action === 'ignore' ? { ...current, purge: event.target.checked } : current)}
                />
                <span>
                  <span className="block font-semibold text-red-600">同时彻底删除这些账号及关联数据</span>
                  <span className="mt-1 block text-xs leading-relaxed text-muted-foreground">清理未结算利润账本、用量日志、账号级统计和审查事件。API Key 已累计总额度不会回退；存在结算草稿或已确认结算时会拒绝删除。</span>
                </span>
              </label>
            </div>
            <DialogFooter>
              <Button variant="outline" disabled={busy === 'pending-action'} onClick={() => setPendingActionDialog(null)}>取消</Button>
              <Button variant={pendingActionDialog.purge ? 'destructive' : 'default'} disabled={busy === 'pending-action'} onClick={submitPendingAction}>
                {busy === 'pending-action' ? <Loader2 className="size-4 animate-spin" /> : pendingActionDialog.purge ? <Trash2 className="size-4" /> : <EyeOff className="size-4" />}
                {busy === 'pending-action' && pendingActionProgress
                  ? `正在处理 ${pendingActionProgress.completed}/${pendingActionProgress.total}`
                  : pendingActionDialog.purge ? '继续永久删除确认' : `确认忽略 ${pendingActionDialog.accounts.length} 个账号`}
              </Button>
            </DialogFooter>
          </> : null}

          {pendingActionDialog?.action === 'ignore' && pendingActionDialog.stage === 'confirm-purge' ? <>
            <DialogHeader>
              <DialogTitle className="text-red-600">二次确认：永久删除</DialogTitle>
              <DialogDescription>
                此操作不可恢复。下面 {pendingActionDialog.accounts.length} 个账号及其未结算关联数据将按顺序分批清理。
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              <PendingAccountConfirmationList accounts={pendingActionDialog.accounts} />
              <div className="space-y-2">
                <label className="text-sm font-medium">请输入“{PURGE_CONFIRMATION_TEXT}”以继续</label>
                <Input autoFocus value={pendingActionDialog.confirmation} onChange={(event) => setPendingActionDialog((current) => current?.action === 'ignore' ? { ...current, confirmation: event.target.value } : current)} />
              </div>
            </div>
            <DialogFooter>
              <Button variant="outline" disabled={busy === 'pending-action'} onClick={() => setPendingActionDialog((current) => current?.action === 'ignore' ? { ...current, stage: 'options', confirmation: '' } : current)}>返回</Button>
              <Button variant="destructive" disabled={busy === 'pending-action' || pendingActionDialog.confirmation.trim() !== PURGE_CONFIRMATION_TEXT} onClick={submitPendingAction}>
                {busy === 'pending-action' ? <Loader2 className="size-4 animate-spin" /> : <Trash2 className="size-4" />}
                {busy === 'pending-action' && pendingActionProgress
                  ? `正在删除 ${pendingActionProgress.completed}/${pendingActionProgress.total}`
                  : `确认永久删除 ${pendingActionDialog.accounts.length} 个账号`}
              </Button>
            </DialogFooter>
          </> : null}
        </DialogContent>
      </Dialog>
    </div>
  )
}

function PendingAccountConfirmationList({ accounts }: { accounts: ProfitPendingAccount[] }) {
  const totalRequests = accounts.reduce((sum, account) => sum + account.pending_requests, 0)
  const totalCost = accounts.reduce((sum, account) => sum + account.official_cost_usd_micros, 0)
  return (
    <div className="overflow-hidden rounded-xl border border-border">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border bg-muted/30 px-3 py-2 text-xs text-muted-foreground">
        <span>账号清单 · {accounts.length} 个</span>
        <span>{formatNumber(totalRequests)} 次请求 · {formatUSD(totalCost)}</span>
      </div>
      <div className="max-h-64 divide-y divide-border overflow-y-auto">
        {accounts.map((account) => <div key={account.account_id} className="flex items-start justify-between gap-3 px-3 py-2.5 text-sm">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2 font-medium">
              <span className="truncate">{account.account_name || `账号 #${account.account_id}`}</span>
              <Badge variant="outline">#{account.account_id}</Badge>
              {account.deleted ? <Badge variant="destructive">已删除</Badge> : null}
            </div>
            <div className="mt-1 text-xs text-muted-foreground">
              原有分组：{account.operational_groups.length > 0
                ? account.operational_groups.map((group) => group.name).join('、')
                : '无'}
            </div>
          </div>
          <div className="shrink-0 text-right text-xs text-muted-foreground">
            <div>{formatNumber(account.pending_requests)} 次</div>
            <div className="mt-1">{formatUSD(account.official_cost_usd_micros)}</div>
          </div>
        </div>)}
      </div>
    </div>
  )
}

function MetricCard({ title, value, hint, positive }: { title: string; value: string; hint: string; positive?: boolean }) {
  return <Card><CardContent className="p-5"><div className="text-sm font-medium text-muted-foreground">{title}</div><div className={cn('mt-2 text-2xl font-bold tabular-nums', positive === true && 'text-emerald-600', positive === false && 'text-red-500')}>{value}</div><div className="mt-1 text-xs text-muted-foreground">{hint}</div></CardContent></Card>
}

function DimensionTable({ rows }: { rows: ProfitDashboardDimension[] }) {
  return <div className="overflow-x-auto"><Table><TableHeader><TableRow><TableHead>名称</TableHead><TableHead className="text-right">请求</TableHead><TableHead className="text-right">Token</TableHead><TableHead className="text-right">官方用量</TableHead></TableRow></TableHeader><TableBody>{rows.length === 0 ? <TableRow><TableCell colSpan={4} className="py-10 text-center text-muted-foreground">该范围暂无数据</TableCell></TableRow> : rows.map((row) => <TableRow key={row.id}><TableCell><div className="flex items-center gap-2 font-medium">{row.name || `#${row.id}`}{row.deleted ? <Badge variant="destructive">已删除</Badge> : null}{row.pending ? <Badge variant="secondary">待确认</Badge> : null}</div></TableCell><TableCell className="text-right tabular-nums">{formatNumber(row.request_count)}</TableCell><TableCell className="text-right tabular-nums">{formatNumber(row.total_tokens)}</TableCell><TableCell className="text-right tabular-nums">{formatUSD(row.official_cost_usd_micros)}</TableCell></TableRow>)}</TableBody></Table></div>
}

function GroupSettlementTable({ rows }: { rows: ProfitDashboardResponse['group_settlements'] }) {
  return <div className="overflow-x-auto"><Table><TableHeader><TableRow><TableHead>分组</TableHead><TableHead className="text-right">自己账号用量</TableHead><TableHead className="text-right">应收</TableHead><TableHead className="text-right">应付</TableHead><TableHead className="text-right">净额</TableHead></TableRow></TableHeader><TableBody>{rows.length === 0 ? <TableRow><TableCell colSpan={5} className="py-10 text-center text-muted-foreground">该范围暂无可结算分组</TableCell></TableRow> : rows.map((row) => <TableRow key={row.group_id}><TableCell className="font-medium">{row.group_name}</TableCell><TableCell className="text-right tabular-nums">{formatUSD(row.self_usage_usd_micros)}</TableCell><TableCell className="text-right font-semibold tabular-nums text-emerald-600">{formatCNY(row.receivable_cny_micros)}</TableCell><TableCell className="text-right font-semibold tabular-nums text-amber-600">{formatCNY(row.payable_cny_micros)}</TableCell><TableCell className={cn('text-right font-semibold tabular-nums', row.net_cny_micros > 0 && 'text-emerald-600', row.net_cny_micros < 0 && 'text-amber-600')}>{formatCNY(row.net_cny_micros)}</TableCell></TableRow>)}</TableBody></Table></div>
}

function SettlementMatrixTable({ rows }: { rows: ProfitDashboardResponse['settlement_matrix'] }) {
  return <div className="overflow-x-auto"><Table><TableHeader><TableRow><TableHead>使用方</TableHead><TableHead>账号所有方</TableHead><TableHead className="text-right">官方用量</TableHead><TableHead className="text-right">结算比例</TableHead><TableHead className="text-right">应付 / 应收</TableHead><TableHead className="text-right">请求</TableHead></TableRow></TableHeader><TableBody>{rows.length === 0 ? <TableRow><TableCell colSpan={6} className="py-10 text-center text-muted-foreground">该范围没有跨分组占用</TableCell></TableRow> : rows.map((row) => <TableRow key={`${row.consumer_group_id}-${row.owner_group_id}`}><TableCell className="font-medium">{row.consumer_group_name}</TableCell><TableCell className="font-medium">{row.owner_group_name}</TableCell><TableCell className="text-right tabular-nums">{formatUSD(row.official_cost_usd_micros)}</TableCell><TableCell className="text-right tabular-nums">{formatRate(row.rate_ppm)}</TableCell><TableCell className="text-right font-semibold tabular-nums">{formatCNY(row.payable_cny_micros)}</TableCell><TableCell className="text-right tabular-nums">{formatNumber(row.request_count)}</TableCell></TableRow>)}</TableBody></Table></div>
}

function AccountROITable({ rows }: { rows: ProfitDashboardResponse['account_roi'] }) {
  return <div className="overflow-x-auto"><Table><TableHeader><TableRow><TableHead>账号</TableHead><TableHead>所有方</TableHead><TableHead>月份</TableHead><TableHead className="text-right">范围用量</TableHead><TableHead className="text-right">月固定成本</TableHead><TableHead className="text-right">本次回收</TableHead><TableHead className="text-right">尚未回收</TableHead><TableHead className="text-right">额度利用率</TableHead></TableRow></TableHeader><TableBody>{rows.length === 0 ? <TableRow><TableCell colSpan={8} className="py-10 text-center text-muted-foreground">该范围暂无账号成本数据</TableCell></TableRow> : rows.map((row) => <TableRow key={`${row.account_id}-${row.owner_group_id}-${row.effective_month}`}><TableCell><div className="flex items-center gap-2 font-medium">{row.account_name || `账号 #${row.account_id}`}{row.account_deleted ? <Badge variant="destructive">已删除</Badge> : null}</div></TableCell><TableCell>{row.owner_group_name}</TableCell><TableCell>{row.effective_month.slice(0, 7)}</TableCell><TableCell className="text-right tabular-nums">{formatUSD(row.usage_in_manifest_usd_micros)}</TableCell><TableCell className="text-right tabular-nums">{formatUSD(row.monthly_fixed_cost_usd_micros)}</TableCell><TableCell className="text-right font-semibold tabular-nums text-emerald-600">{formatUSD(row.allocated_in_range_usd_micros)}</TableCell><TableCell className="text-right tabular-nums">{formatUSD(row.remaining_fixed_cost_usd_micros)}</TableCell><TableCell className="text-right tabular-nums">{formatPercentPPM(row.utilization_ppm)}</TableCell></TableRow>)}</TableBody></Table></div>
}

function EmptyState({ text }: { text: string }) {
  return <div className="flex flex-col items-center py-12 text-center text-sm text-muted-foreground"><CircleDollarSign className="mb-3 size-8 opacity-50" />{text}</div>
}

function StatusBadge({ status }: { status: string }) {
  if (status === 'confirmed') return <Badge className="bg-emerald-600">已确认</Badge>
  if (status === 'superseded') return <Badge variant="secondary">已被修订</Badge>
  if (status === 'building') return <Badge variant="secondary">正在分批生成</Badge>
  if (status === 'build_failed') return <Badge variant="destructive">生成失败</Badge>
  return <Badge variant="outline">草稿</Badge>
}

function formatRate(ratePPM: number) {
  return `${new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 6 }).format(ratePPM / PPM)} CNY / USD`
}

function formatPercentPPM(value: number) {
  return `${new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 2 }).format(value / 10_000)}%`
}

function assignmentSourceLabel(source: string) {
  if (source === 'manual') return '手动确认'
  if (source === 'legacy_key_group') return '原 Key 分组'
  if (source === 'suggested') return '系统建议'
  return source || '未设置'
}
