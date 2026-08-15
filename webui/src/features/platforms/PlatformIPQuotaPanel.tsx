import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, Gauge, ShieldAlert, TrendingUp } from "lucide-react";
import { Badge } from "../../components/ui/Badge";
import { Card } from "../../components/ui/Card";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { formatGoDuration } from "../../lib/time";
import { getPlatformIPQuota } from "./api";
import type { Platform, PlatformIPQuotaIPEntry } from "./types";

const QUOTA_REFRESH_MS = 5_000;
const MAX_VISIBLE_IPS = 50;

function sortIPsByAccounts(ips: PlatformIPQuotaIPEntry[]): PlatformIPQuotaIPEntry[] {
  return [...ips].sort((left, right) => {
    if (right.accounts !== left.accounts) {
      return right.accounts - left.accounts;
    }
    return left.ip.localeCompare(right.ip);
  });
}

export function PlatformIPQuotaPanel({ platform }: { platform: Platform }) {
  const { t } = useI18n();

  const quotaQuery = useQuery({
    queryKey: ["platform-ip-quota", platform.id],
    queryFn: () => getPlatformIPQuota(platform.id),
    refetchInterval: QUOTA_REFRESH_MS,
    placeholderData: (previous) => previous,
  });

  const quota = quotaQuery.data;
  const enabled = quota?.enabled ?? platform.max_accounts_per_ip > 0;
  const maxAccounts = quota?.max_accounts_per_ip ?? platform.max_accounts_per_ip;
  const windowLabel = quota ? formatGoDuration(quota.ip_account_window) : t("默认");
  const sortedIPs = sortIPsByAccounts(quota?.ips ?? []);
  const visibleIPs = sortedIPs.slice(0, MAX_VISIBLE_IPS);

  return (
    <Card className="dashboard-panel platform-monitor-span-2 platform-ip-quota-panel">
      <div className="dashboard-panel-header">
        <h3>{t("IP 账号配额")}</h3>
        <p>{t("各出口 IP 在统计窗口内的去重账号数")}</p>
      </div>

      {quotaQuery.isError ? (
        <div className="callout callout-error">
          <AlertTriangle size={14} />
          <span>{formatApiErrorMessage(quotaQuery.error, t)}</span>
        </div>
      ) : null}

      {!enabled && !quotaQuery.isError ? (
        <div className="empty-box dashboard-empty">
          <Gauge size={14} />
          <p>{t("未启用：在“配置”页设置单 IP 最大账号数后生效")}</p>
        </div>
      ) : null}

      {enabled && !quotaQuery.isError ? (
        <>
          <div className="platform-monitor-snapshot-list platform-ip-quota-summary">
            <div>
              <span>{t("单 IP 上限")}</span>
              <p>{maxAccounts}</p>
            </div>
            <div>
              <span>{t("统计窗口")}</span>
              <p>{windowLabel}</p>
            </div>
            <div>
              <span>{t("满额 IP 数")}</span>
              <p>
                {sortedIPs.filter((item) => item.accounts >= maxAccounts).length}
                <small> / {sortedIPs.length}</small>
              </p>
            </div>
          </div>

          <div className="platform-ip-quota-metrics">
            <span>
              <ShieldAlert size={13} />
              {t("配额阻断")} {quota?.ip_quota_blocked_total ?? 0}
            </span>
            <span>
              <TrendingUp size={13} />
              {t("兜底放行")} {quota?.ip_quota_fallback_total ?? 0}
            </span>
          </div>

          {visibleIPs.length ? (
            <div className="platform-ip-quota-table">
              <div className="platform-ip-quota-row platform-ip-quota-row-head">
                <span>{t("出口 IP")}</span>
                <span>{t("窗口内账号数")}</span>
                <span>{t("使用率")}</span>
              </div>
              {visibleIPs.map((item) => {
                const ratio = maxAccounts > 0 ? item.accounts / maxAccounts : 0;
                const full = item.accounts >= maxAccounts;
                return (
                  <div key={item.ip} className="platform-ip-quota-row">
                    <span className="platform-ip-quota-ip" title={item.ip}>
                      {item.ip}
                    </span>
                    <span>
                      {item.accounts} / {maxAccounts}
                    </span>
                    <span className="platform-ip-quota-usage">
                      <i
                        className={`platform-ip-quota-bar ${full ? "platform-ip-quota-bar-full" : ""}`}
                        style={{ width: `${Math.min(100, Math.round(ratio * 100))}%` }}
                      />
                      {full ? (
                        <Badge variant="warning">{t("满额")}</Badge>
                      ) : (
                        <small>{Math.round(ratio * 100)}%</small>
                      )}
                    </span>
                  </div>
                );
              })}
              {sortedIPs.length > visibleIPs.length ? (
                <p className="muted platform-ip-quota-more">
                  {t("仅展示账号数最多的前 {{count}} 个 IP", { count: visibleIPs.length })}
                </p>
              ) : null}
            </div>
          ) : (
            <div className="empty-box dashboard-empty">
              <Gauge size={14} />
              <p>{t("统计窗口内暂无账号绑定记录")}</p>
            </div>
          )}
        </>
      ) : null}
    </Card>
  );
}
