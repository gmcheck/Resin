import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, ExternalLink, Gauge, ShieldAlert, TrendingUp, X } from "lucide-react";
import { useEffect, useState } from "react";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { formatDateTime, formatGoDuration, formatRelativeTime } from "../../lib/time";
import { getPlatformIPQuota } from "./api";
import type { Platform, PlatformIPQuotaAccountDetail, PlatformIPQuotaIPEntry } from "./types";

const QUOTA_REFRESH_MS = 5_000;
const MAX_VISIBLE_IPS = 50;

// Cross-tab navigation target: opening an IP modal from the lease table.
export type QuotaFocusTarget = {
  ip: string;
  seq: number;
};

function sortIPsByAccounts(ips: PlatformIPQuotaIPEntry[]): PlatformIPQuotaIPEntry[] {
  return [...ips].sort((left, right) => {
    if (right.accounts !== left.accounts) {
      return right.accounts - left.accounts;
    }
    return left.ip.localeCompare(right.ip);
  });
}

// Leased accounts first, then most recently active.
function sortAccountDetails(details: PlatformIPQuotaAccountDetail[]): PlatformIPQuotaAccountDetail[] {
  return [...details].sort((left, right) => {
    if (left.has_lease !== right.has_lease) {
      return left.has_lease ? -1 : 1;
    }
    return right.last_seen_ns - left.last_seen_ns;
  });
}

function AccountStatusBadges({ detail }: { detail: PlatformIPQuotaAccountDetail }) {
  const { t } = useI18n();
  if (detail.has_lease) {
    return (
      <Badge variant="success" title={t("账号当前租约绑定该出口 IP")}>
        {t("使用中")}
      </Badge>
    );
  }
  return (
    <Badge variant="muted" title={t("账号已不再使用该 IP，名额将在滑出统计窗口后释放")}>
      {t("窗口残留")}
    </Badge>
  );
}

function IPAccountsModal({
  entry,
  maxAccounts,
  windowLabel,
  onManageLease,
  onClose,
}: {
  entry: PlatformIPQuotaIPEntry;
  maxAccounts: number;
  windowLabel: string;
  onManageLease?: (account: string) => void;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const details = sortAccountDetails(entry.account_details);
  const full = maxAccounts > 0 && entry.accounts >= maxAccounts;

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        onClose();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [onClose]);

  return (
    <div
      className="modal-overlay"
      role="dialog"
      aria-modal="true"
      aria-label={t("出口 IP 账号明细")}
      onClick={(event) => {
        if (event.target === event.currentTarget) {
          onClose();
        }
      }}
    >
      <Card className="modal-card platform-ip-accounts-modal">
        <div className="modal-header">
          <div>
            <h3>{entry.ip}</h3>
            <p>
              {t("窗口内账号 {{count}} / {{max}} · 统计窗口 {{window}}", {
                count: entry.accounts,
                max: maxAccounts,
                window: windowLabel,
              })}
              {full ? ` · ${t("满额")}` : ""}
            </p>
          </div>
          <Button variant="ghost" size="sm" onClick={onClose} aria-label={t("关闭")}>
            <X size={16} />
          </Button>
        </div>

        <div className="platform-ip-accounts-table">
          <div className="platform-ip-accounts-row platform-ip-accounts-row-head">
            <span>{t("账号")}</span>
            <span>{t("状态")}</span>
            <span>{t("最后活跃")}</span>
          </div>
          {details.map((detail) => (
            <div key={detail.account} className="platform-ip-accounts-row">
              <span className="platform-ip-accounts-account">
                <span className="platform-ip-accounts-account-name" title={detail.account}>
                  {detail.account}
                </span>
                {detail.has_lease && onManageLease ? (
                  <button
                    type="button"
                    className="platform-ip-accounts-manage"
                    title={t("跳转到租约管理并搜索该账号")}
                    onClick={() => onManageLease(detail.account)}
                  >
                    <ExternalLink size={12} />
                    {t("租约")}
                  </button>
                ) : null}
              </span>
              <span className="platform-ip-accounts-status">
                <AccountStatusBadges detail={detail} />
                {detail.via_fallback ? (
                  <Badge variant="warning" title={t("所有出口 IP 均满额时兜底放行，占用 N+1 硬上限槽位")}>
                    {t("兜底超额")}
                  </Badge>
                ) : null}
              </span>
              <span title={formatDateTime(detail.last_seen)}>{formatRelativeTime(detail.last_seen)}</span>
            </div>
          ))}
          {!details.length ? <p className="muted platform-ip-accounts-empty">{t("该 IP 在统计窗口内暂无账号记录")}</p> : null}
        </div>

        <p className="muted platform-ip-accounts-legend">
          {t(
            "使用中：账号租约当前绑定该 IP；窗口残留：账号已无租约，最长再暴露一个统计窗口后释放名额；兜底超额：占用 fail-open 的 N+1 槽位。",
          )}
        </p>
      </Card>
    </div>
  );
}

export function PlatformIPQuotaPanel({
  platform,
  focusTarget = null,
  onManageLease,
}: {
  platform: Platform;
  focusTarget?: QuotaFocusTarget | null;
  onManageLease?: (account: string) => void;
}) {
  const { t } = useI18n();
  const [selectedIP, setSelectedIP] = useState<string | null>(null);
  const [appliedFocusSeq, setAppliedFocusSeq] = useState(0);

  // Cross-tab focus (lease table → IP modal): applied during render so the
  // modal opens without an effect round-trip; seq handles re-clicking the same IP.
  if (focusTarget && focusTarget.seq !== appliedFocusSeq) {
    setAppliedFocusSeq(focusTarget.seq);
    setSelectedIP(focusTarget.ip);
  }

  const quotaQuery = useQuery({
    queryKey: ["platform-ip-quota", platform.id],
    queryFn: () => getPlatformIPQuota(platform.id),
    refetchInterval: QUOTA_REFRESH_MS,
    placeholderData: (previous) => previous,
  });

  const quota = quotaQuery.data;
  const enabled = quota?.enabled ?? platform.max_accounts_per_ip > 0;
  const maxAccounts = quota?.max_accounts_per_ip ?? platform.max_accounts_per_ip;
  const windowLabel = quota ? formatGoDuration(quota.ip_account_window, t("默认")) : t("默认");
  const sortedIPs = sortIPsByAccounts(quota?.ips ?? []);
  const visibleIPs = sortedIPs.slice(0, MAX_VISIBLE_IPS);
  // Live lookup keeps the modal fresh with the 5s query refresh; if the IP
  // slides out of the window while open, the modal degrades to an empty state.
  const selectedEntry = selectedIP
    ? sortedIPs.find((item) => item.ip === selectedIP) ?? { ip: selectedIP, accounts: 0, account_details: [] }
    : null;

  const closeModal = () => setSelectedIP(null);

  return (
    <Card className="dashboard-panel platform-monitor-span-2 platform-ip-quota-panel">
      <div className="dashboard-panel-header">
        <h3>{t("IP 账号配额")}</h3>
        <p>{t("各出口 IP 在统计窗口内的去重账号数，点击 IP 查看绑定账号")}</p>
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
                    <button
                      type="button"
                      className="platform-ip-quota-ip-link"
                      title={t("查看该 IP 绑定的账号")}
                      onClick={() => setSelectedIP(item.ip)}
                    >
                      {item.ip}
                    </button>
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

      {selectedEntry ? (
        <IPAccountsModal
          entry={selectedEntry}
          maxAccounts={maxAccounts}
          windowLabel={windowLabel}
          onManageLease={onManageLease}
          onClose={closeModal}
        />
      ) : null}
    </Card>
  );
}
