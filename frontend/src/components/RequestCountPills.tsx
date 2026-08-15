import { useTranslation } from "react-i18next";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import type { AccountRow } from "../types";
import {
  buildErrorStatusBreakdown,
  formatErrorStatusPercent,
} from "../lib/requestErrorStatus";

export default function RequestCountPills({
  account,
  compact = false,
}: {
  account: AccountRow;
  compact?: boolean;
}) {
  const { t } = useTranslation();
  const success = account.success_requests ?? 0;
  const errors = account.error_requests ?? 0;
  const retryErrors = account.retry_error_requests ?? 0;
  const rateLimits = account.rate_limit_attempts ?? 0;
  const breakdown = buildErrorStatusBreakdown(account.error_status_counts, errors);
  const idle = success === 0 && errors === 0;
  const pad = compact ? "h-6 min-w-[2.25rem] px-2 text-[11px]" : "h-7 min-w-[2.5rem] px-2.5 text-[12px]";

  return (
    <div className="flex flex-col items-start gap-1">
      <div
        className={cn(
          "inline-flex items-center rounded-full p-0.5 ring-1 ring-inset ring-border/70",
          idle ? "bg-muted/50" : "bg-muted/30",
        )}
      >
        <span
          className={cn(
            "inline-flex items-center justify-center gap-1 rounded-full font-mono font-semibold tabular-nums",
            pad,
            success > 0
              ? "bg-emerald-500/12 text-emerald-700 dark:bg-emerald-400/12 dark:text-emerald-300"
              : "text-muted-foreground/70",
          )}
          title={t("accounts.requestSuccess")}
        >
          <span
            className={cn(
              "size-1.5 shrink-0 rounded-full",
              success > 0 ? "bg-emerald-500" : "bg-muted-foreground/30",
            )}
            aria-hidden
          />
          {success.toLocaleString()}
        </span>

        {errors > 0 ? (
          <TooltipProvider delayDuration={120}>
            <Tooltip>
              <TooltipTrigger asChild>
                <span
                  tabIndex={0}
                  className={cn(
                    "inline-flex cursor-help items-center justify-center gap-1 rounded-full bg-red-500/12 font-mono font-semibold tabular-nums text-red-700 transition-colors hover:bg-red-500/18 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-red-400/50 dark:bg-red-400/12 dark:text-red-300 dark:hover:bg-red-400/18",
                    pad,
                  )}
                  aria-label={t("accounts.requestErrorTooltipAria", { count: errors })}
                >
                  <span
                    className="size-1.5 shrink-0 rounded-full bg-red-500"
                    aria-hidden
                  />
                  {errors.toLocaleString()}
                </span>
              </TooltipTrigger>
              <TooltipContent
                side="top"
                sideOffset={10}
                className="min-w-[240px] rounded-xl border border-white/10 bg-zinc-950/95 px-3.5 py-3 text-left text-xs text-zinc-50 shadow-2xl backdrop-blur-md"
              >
                <div className="mb-2 flex items-baseline justify-between gap-3">
                  <span className="text-[11px] font-semibold tracking-wide text-zinc-300">
                    {t("accounts.errorStatusTooltipTitle")}
                  </span>
                  <span className="font-mono text-[11px] tabular-nums text-zinc-500">
                    {errors.toLocaleString()}
                  </span>
                </div>
                {breakdown.length === 0 ? (
                  <div className="text-zinc-500">{t("accounts.errorStatusEmpty")}</div>
                ) : (
                  <div className="space-y-2">
                    {breakdown.map((row) => (
                      <div key={row.code} className="space-y-1">
                        <div className="flex items-baseline justify-between gap-3 font-mono tabular-nums">
                          <span className="rounded-md bg-white/8 px-1.5 py-0.5 text-[11px] font-semibold text-zinc-100">
                            {row.code}
                          </span>
                          <span className="text-zinc-200">{row.count.toLocaleString()}</span>
                          <span className="text-zinc-500">
                            {formatErrorStatusPercent(row.percent)}
                          </span>
                        </div>
                        <div className="h-1 overflow-hidden rounded-full bg-white/8">
                          <div
                            className="h-full rounded-full bg-gradient-to-r from-red-400 to-rose-300"
                            style={{ width: `${Math.max(row.percent, 4)}%` }}
                          />
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </TooltipContent>
            </Tooltip>
          </TooltipProvider>
        ) : (
          <span
            className={cn(
              "inline-flex items-center justify-center gap-1 rounded-full font-mono font-semibold tabular-nums text-muted-foreground/70",
              pad,
            )}
            title={t("accounts.requestError")}
          >
            <span className="size-1.5 shrink-0 rounded-full bg-muted-foreground/30" aria-hidden />
            0
          </span>
        )}
      </div>

      {(retryErrors > 0 || rateLimits > 0) && (
        <div className="flex flex-wrap items-center gap-1 pl-0.5 text-[10px] font-medium text-muted-foreground">
          {retryErrors > 0 ? (
            <span className="rounded-full bg-muted/70 px-1.5 py-0.5">
              retry {retryErrors}
            </span>
          ) : null}
          {rateLimits > 0 ? (
            <span className="rounded-full bg-amber-500/10 px-1.5 py-0.5 text-amber-700 dark:text-amber-300">
              429 {rateLimits}
            </span>
          ) : null}
        </div>
      )}
    </div>
  );
}
