import type { Account } from '@/types'

const normalizeUsageRefreshValue = (value: unknown): string => {
  if (value == null) return ''
  return String(value)
}

const normalizeSnapshotRefreshValue = (value: unknown): unknown => {
  if (Array.isArray(value)) {
    return value.map(normalizeSnapshotRefreshValue)
  }
  if (value && typeof value === 'object') {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>)
        .filter(([, entry]) => entry !== undefined)
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([key, entry]) => [key, normalizeSnapshotRefreshValue(entry)])
    )
  }
  return value
}

const serializeSnapshotRefreshValue = (value: unknown): string => {
  if (value == null) return ''
  return JSON.stringify(normalizeSnapshotRefreshValue(value)) ?? ''
}

const isNonBlankString = (value: unknown): value is string => (
  typeof value === 'string' && value.trim().length > 0
)

export const buildOpenAIAstraPolicyRefreshKey = (account: Pick<Account, 'platform' | 'type' | 'openai_oauth_model_policy' | 'codex_turn_tickets'>): string => {
  if (account.platform !== 'openai' || account.type !== 'oauth') return ''
  return [account.openai_oauth_model_policy, account.codex_turn_tickets].map(serializeSnapshotRefreshValue).join('|')
}

export const buildOpenAIQuotaCreditsRefreshKey = (account: Pick<Account, 'platform' | 'type' | 'extra'>): string => {
  if (account.platform !== 'openai' || account.type !== 'oauth') return ''
  const extra = account.extra
  return [extra?.codex_reset_credit_owner_changed_at_us, extra?.codex_reset_credit_snapshot, extra?.codex_credits_snapshot]
    .map(serializeSnapshotRefreshValue).join('|')
}

export const openAIUsageWindowFields = [
  'codex_usage_observed_at_us', 'codex_usage_updated_at',
  'codex_5h_used_percent', 'codex_5h_reset_at', 'codex_5h_reset_after_seconds', 'codex_5h_window_minutes',
  'codex_7d_used_percent', 'codex_7d_reset_at', 'codex_7d_reset_after_seconds', 'codex_7d_window_minutes'
] as const

export const buildOpenAIUsageRefreshKey = (account: Pick<Account, 'id' | 'platform' | 'type' | 'updated_at' | 'last_used_at' | 'rate_limit_reset_at' | 'extra'>): string => {
  if (account.platform !== 'openai' || account.type !== 'oauth') {
    return ''
  }

  const extra = account.extra ?? {}
  return [
    account.id,
    account.updated_at,
    account.last_used_at,
    account.rate_limit_reset_at,
    extra.codex_reset_credit_owner_changed_at_us,
    ...openAIUsageWindowFields.map(key => extra[key])
  ].map(normalizeUsageRefreshValue).join('|')
}

export const buildGrokUsageRefreshKey = (account: Pick<Account, 'platform' | 'extra'>): string => {
  if (account.platform !== 'grok') {
    return ''
  }

  const extra = account.extra ?? {}
  const usageSnapshot = extra.grok_usage_snapshot
  const canonicalTier = (usageSnapshot as Record<string, unknown> | null | undefined)?.subscription_tier
  const legacyQuotaFallback = isNonBlankString(canonicalTier)
    ? undefined
    : extra.grok_quota_snapshot
  return [
    serializeSnapshotRefreshValue(extra.grok_billing_snapshot),
    serializeSnapshotRefreshValue(usageSnapshot),
    serializeSnapshotRefreshValue(legacyQuotaFallback)
  ].join('|')
}
