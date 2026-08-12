import type { ChangeEvent } from "react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Activity,
  Check,
  Fingerprint,
  FolderOpen,
  Gauge,
  Globe,
  KeyRound,
  Loader2,
  Save,
  ShieldCheck,
  Sliders,
  SlidersHorizontal,
  Tag,
  Users,
  X,
  Zap,
} from "lucide-react";
import type {
  AccountGroup,
  AccountRow,
  CodexFingerprintMode,
  UpdateAccountSchedulerRequest,
} from "../types";
import { api } from "../api";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import {
  Sheet,
  SheetBody,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import ChipInput from "./ChipInput";
import AccountGroupMultiSelect from "./AccountGroupMultiSelect";

function formatSignedNumber(value: number): string {
  if (value > 0) return `+${value}`;
  return String(value);
}

function getDefaultScoreBias(planType?: string | null): number {
  const normalized = (planType || "").toLowerCase();
  if (
    normalized.includes("pro") ||
    normalized.includes("plus") ||
    normalized.includes("team")
  ) {
    return 50;
  }
  return 0;
}

function parseCustomHeadersText(value: string): {
  ok: boolean;
  value: Record<string, string> | null;
} {
  const trimmed = value.trim();
  if (!trimmed) return { ok: true, value: null };
  try {
    const parsed = JSON.parse(trimmed);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
      return { ok: false, value: null };
    }
    const result: Record<string, string> = {};
    for (const [k, v] of Object.entries(parsed)) {
      if (typeof v !== "string") return { ok: false, value: null };
      result[k] = v;
    }
    return { ok: true, value: result };
  } catch {
    return { ok: false, value: null };
  }
}

function formatCustomHeadersText(
  headers?: Record<string, string> | null,
): string {
  if (!headers || Object.keys(headers).length === 0) return "";
  return JSON.stringify(headers, null, 2);
}

interface AccountQuickConfigSheetProps {
  account: AccountRow | null;
  groups: AccountGroup[];
  show: boolean;
  onClose: () => void;
  onSaved: () => void;
}

export default function AccountQuickConfigSheet({
  account,
  groups,
  show,
  onClose,
  onSaved,
}: AccountQuickConfigSheetProps) {
  const { t } = useTranslation();
  const { showToast } = useToast();

  const [saving, setSaving] = useState(false);
  const [fingerprintMode, setFingerprintMode] =
    useState<CodexFingerprintMode>("off");
  const [scoreMode, setScoreMode] = useState<"default" | "custom">("default");
  const [scoreInput, setScoreInput] = useState("");
  const [concurrencyMode, setConcurrencyMode] = useState<
    "default" | "custom"
  >("default");
  const [concurrencyInput, setConcurrencyInput] = useState("");
  const [schedulerPriorityInput, setSchedulerPriorityInput] = useState("");
  const [skipWarmTier, setSkipWarmTier] = useState(false);
  const [proxyUrl, setProxyUrl] = useState("");
  const [customHeadersText, setCustomHeadersText] = useState("");
  const [tags, setTags] = useState<string[]>([]);
  const [groupIds, setGroupIds] = useState<number[]>([]);

  useEffect(() => {
    if (!account) return undefined;

    let cancelled = false;

    const populateForm = (acc: AccountRow) => {
      const mode = (acc.codex_fingerprint_mode as string) || "off";
      setFingerprintMode(
        mode === "device" || mode === "session" || mode === "full" ? mode : "off",
      );
      if (acc.score_bias_override != null) {
        setScoreMode("custom");
        setScoreInput(String(acc.score_bias_override));
      } else {
        setScoreMode("default");
        setScoreInput("");
      }
      if (acc.base_concurrency_override != null) {
        setConcurrencyMode("custom");
        setConcurrencyInput(String(acc.base_concurrency_override));
      } else {
        setConcurrencyMode("default");
        setConcurrencyInput("");
      }
      setSchedulerPriorityInput(
        acc.scheduler_priority != null ? String(acc.scheduler_priority) : "",
      );
      setSkipWarmTier(acc.skip_warm_tier ?? false);
      setProxyUrl(acc.proxy_url ?? "");
      setCustomHeadersText(formatCustomHeadersText(acc.custom_headers));
      setTags(acc.tags ?? []);
      setGroupIds(acc.group_ids ?? []);
    };

    populateForm(account);

    void api.getAccount(account.id)
      .then((detailed) => {
        if (!cancelled && detailed) {
          populateForm(detailed);
        }
      })
      .catch(() => undefined);

    return () => {
      cancelled = true;
    };
  }, [account]);

  if (!account) return null;

  const handleSave = async () => {
    const parsedHeaders = parseCustomHeadersText(customHeadersText);
    if (!parsedHeaders.ok) {
      showToast("自定义请求头必须是 JSON 对象，且所有值必须是字符串", "error");
      return;
    }

    let parsedScoreBias: number | null = null;
    if (scoreMode === "custom") {
      const v = parseInt(scoreInput.trim(), 10);
      if (Number.isNaN(v) || v < -200 || v > 200) {
        showToast("自定义加权分必须在 -200 到 200 之间", "error");
        return;
      }
      parsedScoreBias = v;
    }

    let parsedBaseConcurrency: number | null = null;
    if (concurrencyMode === "custom") {
      const v = parseInt(concurrencyInput.trim(), 10);
      if (Number.isNaN(v) || v < 1) {
        showToast("基础并发覆盖必须大于等于 1", "error");
        return;
      }
      parsedBaseConcurrency = v;
    }

    let parsedSchedulerPriority: number | null = null;
    if (schedulerPriorityInput.trim()) {
      const v = parseInt(schedulerPriorityInput.trim(), 10);
      if (Number.isNaN(v) || v < -100 || v > 100) {
        showToast("调度优先级必须在 -100 到 100 之间", "error");
        return;
      }
      parsedSchedulerPriority = v;
    }

    setSaving(true);
    try {
      const payload: UpdateAccountSchedulerRequest = {
        score_bias_override: scoreMode === "custom" ? parsedScoreBias : null,
        base_concurrency_override:
          concurrencyMode === "custom" ? parsedBaseConcurrency : null,
        scheduler_priority: parsedSchedulerPriority,
        skip_warm_tier: skipWarmTier,
        proxy_url: proxyUrl.trim() || null,
        custom_headers: parsedHeaders.value,
        codex_fingerprint_mode: fingerprintMode,
        tags,
        group_ids: groupIds,
      };

      await api.updateAccountScheduler(account.id, payload);
      showToast("账号配置与设备指纹已保存");
      onSaved();
      onClose();
    } catch (err) {
      showToast(`保存失败: ${getErrorMessage(err)}`, "error");
    } finally {
      setSaving(false);
    }
  };

  const fingerprintOptions: { value: CodexFingerprintMode; label: string }[] = [
    { value: "off", label: t("accounts.codexFingerprintModeOff") },
    { value: "device", label: t("accounts.codexFingerprintModeDevice") },
    { value: "session", label: t("accounts.codexFingerprintModeSession") },
    { value: "full", label: t("accounts.codexFingerprintModeFull") },
  ];

  const fingerprintDetails: Record<CodexFingerprintMode, string> = {
    off: t("accounts.codexFingerprintModeOffDetail"),
    device: t("accounts.codexFingerprintModeDeviceDetail"),
    session: t("accounts.codexFingerprintModeSessionDetail"),
    full: t("accounts.codexFingerprintModeFullDetail"),
  };

  return (
    <Sheet open={show} onOpenChange={(o) => { if (!o) onClose(); }}>
      <SheetContent
        side="left"
        className="sm:w-[min(calc(100%-1.5rem),520px)] sm:max-w-[min(calc(100%-1.5rem),520px)]"
      >
        <SheetHeader>
          <div className="flex items-center gap-2">
            <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/20">
              <Fingerprint className="size-4.5" />
            </div>
            <div className="min-w-0">
              <SheetTitle className="text-base font-bold text-foreground">
                账号指纹与快捷配置
              </SheetTitle>
              <SheetDescription className="truncate text-xs text-muted-foreground">
                {account.email || account.name || `ID ${account.id}`}
              </SheetDescription>
            </div>
          </div>
        </SheetHeader>

        <SheetBody className="space-y-5">
          {/* 账号概览 Header */}
          <div className="rounded-xl border border-border/70 bg-gradient-to-r from-primary/5 via-muted/30 to-background p-3.5 shadow-2xs">
            <div className="flex items-center justify-between gap-2">
              <div className="min-w-0 space-y-0.5">
                <div className="text-xs font-bold text-foreground truncate">
                  {account.email || account.name || `ID ${account.id}`}
                </div>
                <div className="text-[11px] text-muted-foreground">
                  账号 ID: {account.id} · 套餐: {account.plan_type || "Standard"}
                </div>
              </div>
              <Badge variant="outline" className="text-xs font-mono font-semibold">
                {account.status || "active"}
              </Badge>
            </div>
          </div>

          {/* 1. 设备指纹收敛 */}
          <div className="rounded-xl border border-border/70 bg-card p-4 shadow-2xs space-y-3">
            <div className="flex items-center gap-2 border-b border-border/50 pb-2.5">
              <Fingerprint className="size-4 text-teal-500" />
              <span className="text-xs font-bold uppercase tracking-wider text-foreground">
                {t("accounts.codexFingerprintModeTitle")}
              </span>
            </div>
            <p className="text-xs text-muted-foreground leading-relaxed">
              {t("accounts.codexFingerprintModeHint")}
            </p>
            <div className="grid grid-cols-2 gap-1.5 rounded-xl border border-border/70 bg-muted/30 p-1">
              {fingerprintOptions.map((opt) => (
                <button
                  key={opt.value}
                  type="button"
                  onClick={() => setFingerprintMode(opt.value)}
                  className={`rounded-lg px-2.5 py-1.5 text-xs font-semibold transition-all duration-150 ${
                    fingerprintMode === opt.value
                      ? "bg-primary text-primary-foreground font-bold shadow-xs ring-1 ring-primary/30"
                      : "text-muted-foreground hover:bg-background/60 hover:text-foreground"
                  }`}
                >
                  {opt.label}
                </button>
              ))}
            </div>
            <div className="rounded-lg border border-border/50 bg-muted/20 p-2.5 text-xs text-muted-foreground">
              {fingerprintDetails[fingerprintMode]}
            </div>
          </div>

          {/* 2. 调度与并发 */}
          <div className="rounded-xl border border-border/70 bg-card p-4 shadow-2xs space-y-3.5">
            <div className="flex items-center gap-2 border-b border-border/50 pb-2.5">
              <Gauge className="size-4 text-amber-500" />
              <span className="text-xs font-bold uppercase tracking-wider text-foreground">
                调度与并发加权
              </span>
            </div>

            {/* 加权分 */}
            <div className="space-y-2">
              <div className="flex items-center justify-between text-xs font-semibold">
                <span className="text-foreground">{t("accounts.schedulerScoreLabel")}</span>
                <span className="text-muted-foreground text-[11px]">{t("accounts.schedulerScoreHint")}</span>
              </div>
              <div className="flex gap-2">
                <button
                  type="button"
                  onClick={() => setScoreMode("default")}
                  className={`flex-1 rounded-lg px-3 py-1.5 text-xs font-semibold transition-all ${
                    scoreMode === "default"
                      ? "bg-primary text-primary-foreground shadow-2xs"
                      : "bg-muted/60 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {t("accounts.schedulerScoreAuto")}
                </button>
                <button
                  type="button"
                  onClick={() => setScoreMode("custom")}
                  className={`flex-1 rounded-lg px-3 py-1.5 text-xs font-semibold transition-all ${
                    scoreMode === "custom"
                      ? "bg-primary text-primary-foreground shadow-2xs"
                      : "bg-muted/60 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {t("accounts.schedulerCustom")}
                </button>
              </div>
              {scoreMode === "default" ? (
                <div className="rounded-lg border border-border/60 bg-muted/20 px-3 py-2 text-xs text-muted-foreground flex justify-between items-center">
                  <span>跟随套餐默认</span>
                  <span className="font-mono font-bold text-primary">
                    {formatSignedNumber(getDefaultScoreBias(account.plan_type))}
                  </span>
                </div>
              ) : (
                <Input
                  inputMode="numeric"
                  value={scoreInput}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setScoreInput(e.target.value)}
                  placeholder={t("accounts.schedulerScorePlaceholder")}
                />
              )}
            </div>

            {/* 基础并发 */}
            <div className="space-y-2 pt-1 border-t border-border/40">
              <div className="flex items-center justify-between text-xs font-semibold">
                <span className="text-foreground">{t("accounts.schedulerConcurrencyLabel")}</span>
                <span className="text-muted-foreground text-[11px]">{t("accounts.schedulerConcurrencyHint")}</span>
              </div>
              <div className="flex gap-2">
                <button
                  type="button"
                  onClick={() => setConcurrencyMode("default")}
                  className={`flex-1 rounded-lg px-3 py-1.5 text-xs font-semibold transition-all ${
                    concurrencyMode === "default"
                      ? "bg-primary text-primary-foreground shadow-2xs"
                      : "bg-muted/60 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {t("accounts.schedulerConcurrencyAuto")}
                </button>
                <button
                  type="button"
                  onClick={() => setConcurrencyMode("custom")}
                  className={`flex-1 rounded-lg px-3 py-1.5 text-xs font-semibold transition-all ${
                    concurrencyMode === "custom"
                      ? "bg-primary text-primary-foreground shadow-2xs"
                      : "bg-muted/60 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {t("accounts.schedulerCustom")}
                </button>
              </div>
              {concurrencyMode === "default" ? (
                <div className="rounded-lg border border-border/60 bg-muted/20 px-3 py-2 text-xs text-muted-foreground flex justify-between items-center">
                  <span>跟随分组/全局</span>
                  <span className="font-mono font-bold text-primary">
                    {account.base_concurrency_effective ?? 2}
                  </span>
                </div>
              ) : (
                <Input
                  inputMode="numeric"
                  value={concurrencyInput}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setConcurrencyInput(e.target.value)}
                  placeholder={t("accounts.schedulerConcurrencyPlaceholder")}
                />
              )}
            </div>

            {/* 优先级 */}
            <div className="space-y-1.5 pt-1 border-t border-border/40">
              <label className="block text-xs font-semibold text-foreground">
                {t("accounts.schedulerPriorityTitle")}
              </label>
              <Input
                inputMode="numeric"
                value={schedulerPriorityInput}
                onChange={(e: ChangeEvent<HTMLInputElement>) => setSchedulerPriorityInput(e.target.value)}
                placeholder={t("accounts.schedulerPriorityPlaceholder")}
              />
            </div>
          </div>

          {/* 3. 防护与网络 */}
          <div className="rounded-xl border border-border/70 bg-card p-4 shadow-2xs space-y-3.5">
            <div className="flex items-center gap-2 border-b border-border/50 pb-2.5">
              <Globe className="size-4 text-indigo-500" />
              <span className="text-xs font-bold uppercase tracking-wider text-foreground">
                防护与网络代理
              </span>
            </div>

            {/* 跳过预热层级 */}
            <div className="flex items-center justify-between gap-3">
              <div>
                <div className="text-xs font-semibold text-foreground">
                  {t("accounts.schedulerSkipWarmLabel")}
                </div>
                <div className="text-[11px] text-muted-foreground">
                  {t("accounts.schedulerSkipWarmHint")}
                </div>
              </div>
              <Switch
                checked={skipWarmTier}
                onCheckedChange={setSkipWarmTier}
              />
            </div>

            {/* 代理服务器 */}
            <div className="space-y-1.5 pt-1 border-t border-border/40">
              <label className="block text-xs font-semibold text-foreground">
                代理服务器 (Proxy URL)
              </label>
              <Input
                value={proxyUrl}
                onChange={(e: ChangeEvent<HTMLInputElement>) => setProxyUrl(e.target.value)}
                placeholder="http://user:pass@host:port"
              />
            </div>

            {/* 自定义请求头 */}
            <div className="space-y-1.5 pt-1 border-t border-border/40">
              <label className="block text-xs font-semibold text-foreground">
                自定义请求头 JSON
              </label>
              <textarea
                rows={3}
                value={customHeadersText}
                onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setCustomHeadersText(e.target.value)}
                placeholder='{"Chatgpt-Account-Id": "workspace-id"}'
                className="w-full rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs shadow-2xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              />
            </div>
          </div>

          {/* 4. 标签与分组 */}
          <div className="rounded-xl border border-border/70 bg-card p-4 shadow-2xs space-y-3.5">
            <div className="flex items-center gap-2 border-b border-border/50 pb-2.5">
              <Tag className="size-4 text-violet-500" />
              <span className="text-xs font-bold uppercase tracking-wider text-foreground">
                标签与分组
              </span>
            </div>

            <div className="space-y-1.5">
              <label className="block text-xs font-semibold text-foreground">
                {t("accounts.tagsLabel")}
              </label>
              <ChipInput
                value={tags}
                onChange={setTags}
                placeholder={t("accounts.tagsPlaceholder")}
                maxVisible={3}
              />
            </div>

            <div className="space-y-1.5 pt-1 border-t border-border/40">
              <label className="block text-xs font-semibold text-foreground">
                {t("accounts.groupsLabel")}
              </label>
              <AccountGroupMultiSelect
                groups={groups}
                value={groupIds}
                onChange={setGroupIds}
                allLabel={t("accounts.groupsUnbound")}
                selectedLabel={t("accounts.groupsSelected", {
                  count: groupIds.length,
                })}
                placeholder={t("accounts.groupsPlaceholder")}
                emptyLabel={t("accounts.groupsNone")}
                emptyHint={t("accounts.groupsSelectHint")}
              />
            </div>
          </div>
        </SheetBody>

        <SheetFooter>
          <Button variant="outline" onClick={onClose} disabled={saving}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void handleSave()} disabled={saving}>
            {saving ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Save className="size-4" />
            )}
            保存配置
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}
