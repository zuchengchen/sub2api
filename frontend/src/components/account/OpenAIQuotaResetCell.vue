<template>
  <div v-if="visible" class="space-y-1">
    <!--
      Unified action row. Parents that already render their own "local query"
      affordance (e.g. AccountUsageCell's active-sampling refresh) pass it in
      via the #pre-actions slot so the user sees a single row of related
      buttons rather than two near-duplicate "查询" rows.

      The 5h / 7d window bars are deliberately NOT rendered here — the local
      active-sampling display (UsageProgressBar in AccountUsageCell) already
      owns that real estate. This cell queries Codex points and reset credits,
      and lets the operator consume a reset credit if needed.
    -->
    <div class="flex flex-wrap items-center gap-1.5">
      <slot name="pre-actions" />

      <div class="inline-flex items-center gap-1">
        <button
          type="button"
          data-testid="reset-credit-count"
          class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
          :disabled="loading || resetting"
          :title="countButtonTitle"
          @click="handleQuery()"
        >
          <svg
            class="h-2.5 w-2.5"
            :class="{ 'animate-spin': loading }"
            fill="none"
            stroke="currentColor"
            viewBox="0 0 24 24"
          >
            <path
              stroke-linecap="round"
              stroke-linejoin="round"
              stroke-width="2"
              d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
            />
          </svg>
          {{ t('admin.accounts.openaiQuotaReset.count') }}<span v-if="data"> {{ availableResetCount }}</span>
        </button>
        <button
          type="button"
          role="switch"
          data-testid="auto-reset-credit-enabled-toggle"
          :aria-checked="autoResetEnabled"
          :aria-label="t('admin.accounts.autoResetCredit.title')"
          :title="autoResetToggleTitle"
          :disabled="autoResetToggling || resetting"
          :class="[
            'relative inline-flex h-4 w-7 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-1 disabled:cursor-not-allowed disabled:opacity-50',
            autoResetEnabled ? 'bg-amber-500' : 'bg-gray-200 dark:bg-dark-600'
          ]"
          @click="handleAutoResetToggle"
        >
          <span
            :class="[
              'pointer-events-none inline-block h-3 w-3 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out',
              autoResetEnabled ? 'translate-x-3' : 'translate-x-0'
            ]"
          />
        </button>
      </div>

      <button
        type="button"
        data-testid="openai-quota-reset"
        class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium text-orange-600 transition-colors hover:bg-orange-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-orange-400 dark:hover:bg-orange-900/30"
        :disabled="resetting || loading || !canReset"
        :title="resetButtonTitle"
        @click="openResetConfirm"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': resetting }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M20 12a8 8 0 11-2.343-5.657L20 8m0 0V4m0 4h-4"
          />
        </svg>
        {{ t('admin.accounts.openaiQuotaReset.reset') }}
      </button>

      <button
        type="button"
        data-testid="codex-credits"
        class="inline-flex max-w-full items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 transition-colors hover:bg-emerald-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-emerald-400 dark:hover:bg-emerald-900/30"
        :disabled="loading || resetting"
        :title="creditsButtonTitle"
        @click="handleQuery()"
      >
        {{ t('admin.accounts.openaiQuotaReset.points') }}
        <span class="truncate tabular-nums">{{ creditsDisplay }}</span>
      </button>
      <!-- 额度用尽后是否允许消耗点数：默认关闭，开启需二次确认 -->
      <button
        type="button"
        role="switch"
        data-testid="openai-credits-enabled-toggle"
        :aria-checked="creditsEnabled"
        :aria-label="t('admin.accounts.openai.creditsEnabled')"
        :title="creditsToggleTitle"
        :disabled="creditsToggling"
        :class="[
          'relative inline-flex h-4 w-7 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-1 disabled:cursor-not-allowed disabled:opacity-50',
          creditsEnabled ? 'bg-amber-500' : 'bg-gray-200 dark:bg-dark-600'
        ]"
        @click="handleCreditsToggle"
      >
        <span
          :class="[
            'pointer-events-none inline-block h-3 w-3 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out',
            creditsEnabled ? 'translate-x-3' : 'translate-x-0'
          ]"
        />
      </button>
      <OpenAIReferralCell :account="account" />
    </div>

    <div v-if="creditsCacheWarning" class="text-[10px] text-amber-600 dark:text-amber-400">
      {{ t('admin.accounts.openaiQuotaReset.pointsCachePersistFailed') }}
    </div>

    <div
      v-if="autoResetState"
      class="flex flex-wrap items-center gap-1 text-[10px]"
      data-testid="auto-reset-credit-state"
    >
      <span
        class="inline-flex items-center rounded px-1.5 py-0.5 font-medium"
        :class="autoResetStateClass"
      >
        {{ autoResetStateLabel }}
        <span v-if="autoResetState.trigger_window" class="ml-1 tabular-nums">
          {{ autoResetState.trigger_window }}
        </span>
      </span>
      <span v-if="autoResetState.checked_at" class="text-gray-500 dark:text-gray-400">
        {{ formatResetCreditExpiry(autoResetState.checked_at, 'short') }}
      </span>
      <span
        v-if="autoResetState.error_code"
        class="max-w-full truncate text-red-600 dark:text-red-400"
        :title="autoResetState.error_code"
      >
        {{ autoResetState.error_code }}
      </span>
    </div>

    <div v-if="primaryResetCreditExpiry" class="space-y-1">
      <div class="flex flex-wrap items-center gap-1">
        <span
          class="inline-flex max-w-full items-center rounded bg-gray-100 px-1.5 py-0.5 text-[10px] leading-4 text-gray-600 tabular-nums dark:bg-dark-800 dark:text-gray-300"
          :title="t('admin.accounts.openaiQuotaReset.expiresAtFull', { time: formatResetCreditExpiry(primaryResetCreditExpiry, 'full') })"
        >
          {{ t('admin.accounts.openaiQuotaReset.expiresAt', { time: formatResetCreditExpiry(primaryResetCreditExpiry, 'short') }) }}
        </span>
        <button
          v-if="hiddenResetCreditCount > 0"
          type="button"
          data-testid="reset-credit-expiry-toggle"
          class="inline-flex items-center rounded-full bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium leading-4 text-gray-600 transition-colors hover:bg-gray-200 dark:bg-dark-800 dark:text-gray-300 dark:hover:bg-dark-700"
          :aria-expanded="showResetCreditDetails"
          :aria-label="resetCreditDetailsToggleLabel"
          :title="resetCreditDetailsTitle"
          @click="toggleResetCreditDetails"
        >
          +{{ hiddenResetCreditCount }}
        </button>
      </div>

      <div
        v-if="showResetCreditDetails && resetCreditExpirations.length > 1"
        data-testid="reset-credit-expiry-details"
        class="inline-grid max-w-full gap-0.5 rounded border border-gray-200 bg-white px-1.5 py-1 text-[10px] leading-4 text-gray-600 shadow-sm dark:border-dark-700 dark:bg-dark-900 dark:text-gray-300"
      >
        <span class="sr-only">{{ t('admin.accounts.openaiQuotaReset.expirationDetails') }}</span>
        <span
          v-for="(expiresAt, index) in resetCreditExpirations"
          :key="`${expiresAt}-${index}`"
          class="flex min-w-0 items-center gap-1 tabular-nums"
          :title="t('admin.accounts.openaiQuotaReset.expiresAtFull', { time: formatResetCreditExpiry(expiresAt, 'full') })"
        >
          <span class="h-1 w-1 shrink-0 rounded-full bg-gray-400 dark:bg-dark-500" />
          <span class="truncate">{{ formatResetCreditExpiry(expiresAt, 'short') }}</span>
        </span>
      </div>
    </div>

    <!-- Error / success feedback -->
    <div
      v-if="error"
      class="text-[10px] text-red-600 dark:text-red-400"
      :title="error"
    >
      {{ truncatedError }}
    </div>
    <div
      v-else-if="resetWarning"
      class="text-[10px] text-amber-600 dark:text-amber-400"
    >
      {{ resetWarning }}
    </div>
    <div
      v-else-if="resetMessage"
      class="text-[10px] text-emerald-600 dark:text-emerald-400"
    >
      {{ resetMessage }}
    </div>

    <ConfirmDialog
      :show="showResetConfirm"
      :title="t('admin.accounts.openaiQuotaReset.confirmTitle')"
      :message="t('admin.accounts.openaiQuotaReset.confirmMessage', { count: availableResetCount })"
      :confirm-text="t('admin.accounts.openaiQuotaReset.reset')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmReset"
      @cancel="showResetConfirm = false"
    />

    <ConfirmDialog
      :show="showCreditsConfirm"
      :title="t('admin.accounts.openai.creditsEnableConfirmTitle')"
      :message="t('admin.accounts.openai.creditsEnableConfirmMessage')"
      :confirm-text="t('admin.accounts.openai.creditsEnableConfirm')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmEnableCredits"
      @cancel="showCreditsConfirm = false"
    />

    <ConfirmDialog
      :show="showAutoResetConfirm"
      :title="t('admin.accounts.autoResetCredit.enableConfirmTitle')"
      :message="autoResetConfirmMessage"
      :confirm-text="t('admin.accounts.autoResetCredit.enableConfirm')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmEnableAutoReset"
      @cancel="showAutoResetConfirm = false"
    />
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'
import {
  refreshOpenAIQuota,
  resetOpenAIQuota,
  setOpenAICreditsEnabled,
  setOpenAIAutoResetCreditEnabled,
  type OpenAIQuotaUsage,
  type OpenAIQuotaResetResult
} from '@/api/admin/accounts'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import OpenAIReferralCell from '@/components/account/OpenAIReferralCell.vue'

const props = defineProps<{
  account: Account
}>()

const emit = defineEmits<{
  'account-updated': [account: Account]
}>()

const { t } = useI18n()

// Visible only for OpenAI OAuth accounts.
const visible = computed(() => props.account.platform === 'openai' && props.account.type === 'oauth')

const loading = ref(false)
const resetting = ref(false)
const error = ref<string | null>(null)
const data = ref<OpenAIQuotaUsage | null>(null)
const cachedData = ref<OpenAIQuotaUsage | null>(null)
const resetMessage = ref<string | null>(null)
const resetWarning = ref<string | null>(null)
const showResetConfirm = ref(false)
const showResetCreditDetails = ref(false)
const creditsCacheWarning = ref(false)
let quotaSnapshotVersion = 0
const autoResetEnabledOverride = ref<boolean | null>(null)
const autoResetToggling = ref(false)
const showAutoResetConfirm = ref(false)
let autoResetToggleVersion = 0

type AutoResetCreditState = NonNullable<NonNullable<Account['extra']>['codex_auto_reset_credit_state']>
const validAutoResetStatuses = new Set(['checking', 'available', 'resetting', 'success', 'no_credit', 'failed'])
const autoResetEnabled = computed(() =>
  autoResetEnabledOverride.value ?? (
    props.account.extra?.auto_reset_credit_enabled === true && props.account.extra?.auto_reset_credit_opt_in_version === 1
  )
)
const autoResetThresholdPercent = (value: unknown) => {
  const threshold = Number(value)
  return Number.isFinite(threshold) && threshold >= 0.001 && threshold <= 1
    ? Number((threshold * 100).toFixed(1)) : 100
}
const autoResetConfirmMessage = computed(() => t('admin.accounts.autoResetCredit.enableConfirmMessage', {
  threshold5h: autoResetThresholdPercent(props.account.extra?.auto_reset_credit_5h_threshold),
  threshold7d: autoResetThresholdPercent(props.account.extra?.auto_reset_credit_7d_threshold)
}))
const autoResetToggleTitle = computed(() =>
  `${t('admin.accounts.autoResetCredit.title')}: ${autoResetEnabled.value ? t('common.enabled') : t('common.disabled')}\n${autoResetConfirmMessage.value}`
)
const autoResetState = computed<AutoResetCreditState | null>(() => {
  if (!autoResetEnabled.value) return null
  const state = props.account.extra?.codex_auto_reset_credit_state
  if (!state || typeof state !== 'object' || !validAutoResetStatuses.has(String(state.status))) return null
  return state
})
const autoResetStateLabel = computed(() => {
  if (!autoResetState.value?.status) return ''
  const keyByStatus: Record<string, string> = {
    checking: 'checking',
    available: 'available',
    resetting: 'resetting',
    success: 'success',
    no_credit: 'noCredit',
    failed: 'failed'
  }
  return t(`admin.accounts.openaiQuotaReset.autoStatus.${keyByStatus[autoResetState.value.status]}`)
})
const autoResetStateClass = computed(() => {
  switch (autoResetState.value?.status) {
    case 'available':
      return 'bg-blue-50 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
    case 'success':
      return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
    case 'no_credit':
    case 'failed':
      return 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300'
    case 'resetting':
      return 'bg-orange-50 text-orange-700 dark:bg-orange-900/30 dark:text-orange-300'
    default:
      return 'bg-gray-100 text-gray-600 dark:bg-dark-800 dark:text-gray-300'
  }
})

const readCachedCredits = (account: Account) => {
  const snapshot = account.extra?.codex_credits_snapshot
  const credits = snapshot?.credits
  if (!credits || typeof credits.has_credits !== 'boolean' || typeof credits.unlimited !== 'boolean') return null
  if (credits.balance != null && typeof credits.balance !== 'string') return null
  return { credits, fetched_at: snapshot.fetched_at }
}
const creditsData = ref(readCachedCredits(props.account))

// 上游余额是十进制字符串，可能超出 Number 精度；按字符串四舍五入到整数，避免大数失真。
const roundDecimalString = (value: string): string | null => {
  const match = /^(\d+)(?:\.(\d*))?$/.exec(value)
  if (!match) {
    const parsed = Number(value)
    return Number.isFinite(parsed) && parsed >= 0 ? String(Math.round(parsed)) : null
  }
  const integer = match[1].replace(/^0+(?=\d)/, '')
  if (!match[2] || match[2][0] < '5') return integer
  // 整数部分 +1（逐位进位）
  const digits = integer.split('')
  let i = digits.length - 1
  while (i >= 0 && digits[i] === '9') {
    digits[i] = '0'
    i--
  }
  if (i < 0) digits.unshift('1')
  else digits[i] = String(Number(digits[i]) + 1)
  return digits.join('')
}

const validCreditsBalance = computed(() => {
  const credits = creditsData.value?.credits
  if (!credits || credits.unlimited || !credits.has_credits) return null
  const balance = credits.balance?.trim()
  if (balance && Number.isFinite(Number(balance)) && Number(balance) >= 0) return balance
  return null
})
const creditsDisplay = computed(() => {
  const credits = creditsData.value?.credits
  if (!credits) return '—'
  if (credits.unlimited) return t('admin.accounts.openaiQuotaReset.pointsUnlimited')
  if (!credits.has_credits) return '0'
  const balance = validCreditsBalance.value
  if (balance) return roundDecimalString(balance) ?? balance
  return t('admin.accounts.openaiQuotaReset.pointsAvailable')
})
const creditsButtonTitle = computed(() => {
  const fetchedAt = creditsData.value?.fetched_at
  const lines = [t('admin.accounts.openaiQuotaReset.pointsTooltip')]
  // 显示为整数，悬停时保留上游原始精确值
  if (validCreditsBalance.value && validCreditsBalance.value !== creditsDisplay.value) {
    lines.push(validCreditsBalance.value)
  }
  if (fetchedAt && Number.isFinite(fetchedAt)) {
    lines.push(t('admin.accounts.openaiQuotaReset.pointsUpdatedAt', {
      time: new Date(fetchedAt * 1000).toLocaleString()
    }))
  }
  return lines.join('\n')
})

const updateCredits = (usage: OpenAIQuotaUsage | null) => {
  creditsData.value = usage?.credits ? { credits: usage.credits, fetched_at: usage.fetched_at } : null
}

// Rehydrate the card from the persisted snapshot. Credits that already expired
// are dropped and the count is clamped to what remains: the snapshot has no
// freshness signal, so an unfiltered read would offer to consume credits that no
// longer exist. A snapshot claiming credits with no usable expiration left is
// treated as absent, which keeps the reset button gated on a live query.
const readCachedResetCredits = (account: Account): OpenAIQuotaUsage | null => {
  const cached = account.extra?.codex_reset_credit_snapshot
  if (!cached || typeof cached !== 'object' || Array.isArray(cached)) return null

  const { available_count: count, credits: rawCredits } = cached as {
    available_count?: unknown
    credits?: unknown
  }
  if (typeof count !== 'number' || !Number.isFinite(count)) return null

  const now = Date.now()
  const credits: { expires_at?: string }[] = []
  if (Array.isArray(rawCredits)) {
    for (const credit of rawCredits) {
      if (!credit || typeof credit !== 'object') continue
      const expiresAt = (credit as { expires_at?: unknown }).expires_at
      if (typeof expiresAt !== 'string' || expiresAt.trim() === '') continue
      const expiryTime = new Date(expiresAt).getTime()
      // Unparsable timestamps are kept: they are already rendered verbatim and
      // dropping them would silently understate the available count.
      if (!Number.isNaN(expiryTime) && expiryTime <= now) continue
      credits.push({ expires_at: expiresAt })
    }
  }
  const availableCount = Math.min(Math.max(count, 0), credits.length)
  // A snapshot that claimed credits but has none left is no longer informative;
  // report "unknown" so the operator re-queries instead of trusting it.
  if (count > 0 && availableCount <= 0) return null
  return {
    fetched_at: 0,
    rate_limit_reset_credits: {
      available_count: availableCount,
      credits
    }
  }
}

cachedData.value = readCachedResetCredits(props.account)
data.value = cachedData.value

const availableResetCount = computed(() => data.value?.rate_limit_reset_credits?.available_count ?? 0)
// Prefer the live payload and fall back to the persisted snapshot only when the
// live state is unknown, so the count and the expirations never come from two
// different generations of the same data.
const resetCreditExpirations = computed(() =>
  ((data.value ?? cachedData.value)?.rate_limit_reset_credits?.credits ?? [])
    .map((credit) => credit.expires_at?.trim() ?? '')
    .filter((expiresAt) => expiresAt.length > 0)
    .sort(compareResetCreditExpiry)
)
const primaryResetCreditExpiry = computed(() => resetCreditExpirations.value[0] ?? '')
const hiddenResetCreditCount = computed(() => Math.max(resetCreditExpirations.value.length - 1, 0))
// A disabled switch can still have an automatic request in flight. Match the
// backend's two-minute guard and expire the local guard without another poll.
const autoResetInProgress = ref(false)
let autoResetGuardTimer: ReturnType<typeof setTimeout> | undefined
watch(
  () => [props.account.id, props.account.extra?.codex_auto_reset_credit_state?.status, props.account.extra?.codex_auto_reset_credit_state?.checked_at, autoResetEnabled.value] as const,
  ([, status, checkedAt, enabled]) => {
    clearTimeout(autoResetGuardTimer)
    autoResetGuardTimer = undefined
    const remaining = Date.parse(checkedAt ?? '') + 120_000 - Date.now()
    autoResetInProgress.value = (status === 'checking' || status === 'resetting') && Number.isFinite(remaining) && remaining > 0
    if (autoResetInProgress.value && !enabled) {
      autoResetGuardTimer = setTimeout(() => { autoResetInProgress.value = false }, remaining)
    }
    if (status === 'resetting') {
      quotaSnapshotVersion++
      data.value = null
      cachedData.value = null
    }
  },
  { immediate: true, flush: 'sync' }
)
onBeforeUnmount(() => clearTimeout(autoResetGuardTimer))
const manualResetBlockedReason = computed(() => {
  if (autoResetToggling.value || showAutoResetConfirm.value) return 'admin.accounts.autoResetCredit.saving'
  if (autoResetEnabled.value) return 'admin.accounts.openaiQuotaReset.resetTooltipAutoEnabled'
  if (autoResetInProgress.value) return 'admin.accounts.openaiQuotaReset.resetTooltipAutoInProgress'
  return ''
})
const canReset = computed(() => availableResetCount.value > 0 && !manualResetBlockedReason.value)
watch(manualResetBlockedReason, (reason) => {
  if (reason) showResetConfirm.value = false
}, { flush: 'sync' })

const resetCreditDetailsTitle = computed(() =>
  resetCreditExpirations.value
    .map((expiresAt) => formatResetCreditExpiry(expiresAt, 'full'))
    .join('\n')
)

const resetCreditDetailsToggleLabel = computed(() => {
  if (showResetCreditDetails.value) {
    return t('admin.accounts.openaiQuotaReset.collapseExpirations')
  }
  return t('admin.accounts.openaiQuotaReset.expandExpirations', { count: hiddenResetCreditCount.value })
})

const resetButtonTitle = computed(() => {
  if (manualResetBlockedReason.value) return t(manualResetBlockedReason.value)
  if (!data.value) return t('admin.accounts.openaiQuotaReset.resetTooltipNeedQuery')
  if (!canReset.value) return t('admin.accounts.openaiQuotaReset.resetTooltipNoCredits')
  return t('admin.accounts.openaiQuotaReset.resetTooltipReady')
})

// "次数" button doubles as the upstream-query trigger and the count display.
// Tooltip differs between "click to load" (no data yet) and "click to refresh".
const countButtonTitle = computed(() => {
  if (!data.value) return t('admin.accounts.openaiQuotaReset.countTooltipLoad')
  return t('admin.accounts.openaiQuotaReset.countTooltipRefresh')
})

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}…` : error.value
})

const getResetCreditExpiryTime = (value: string): number => {
  const time = new Date(value).getTime()
  return Number.isNaN(time) ? Number.POSITIVE_INFINITY : time
}

const compareResetCreditExpiry = (a: string, b: string): number => {
  const diff = getResetCreditExpiryTime(a) - getResetCreditExpiryTime(b)
  if (diff !== 0) return diff
  return a.localeCompare(b)
}

const formatResetCreditExpiry = (value: string, style: 'short' | 'full'): string => {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value

  const options: Intl.DateTimeFormatOptions = {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit'
  }
  if (style === 'full') {
    options.year = 'numeric'
  }

  return new Intl.DateTimeFormat(undefined, options).format(date)
}

const extractErrorMessage = (e: unknown): string => {
  // The project's axios response interceptor (api/client.ts) flattens server
  // errors into { status, code, message, reason, ... } and re-rejects them, so
  // the message lives at the top level rather than under .response.data. Fall
  // back to the raw axios shape for the cancellation/network branches that
  // bypass the flattening, and finally to the generic i18n string.
  const err = e as {
    code?: string
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  if (err?.reason === 'OPENAI_QUOTA_AUTO_RESET_ENABLED' || err?.code === 'OPENAI_QUOTA_AUTO_RESET_ENABLED') {
    return t('admin.accounts.openaiQuotaReset.resetTooltipAutoEnabled')
  }
  if (err?.reason === 'OPENAI_QUOTA_AUTO_RESET_IN_PROGRESS' || err?.code === 'OPENAI_QUOTA_AUTO_RESET_IN_PROGRESS') {
    return t('admin.accounts.openaiQuotaReset.resetTooltipAutoInProgress')
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const toggleResetCreditDetails = () => {
  if (hiddenResetCreditCount.value <= 0) return
  showResetCreditDetails.value = !showResetCreditDetails.value
}

const handleQuery = async () => {
  if (loading.value || resetting.value) return
  const accountID = props.account.id
  const snapshotVersion = quotaSnapshotVersion
  loading.value = true
  creditsCacheWarning.value = false
  error.value = null
  resetMessage.value = null
  resetWarning.value = null
  showResetCreditDetails.value = false
  try {
    const result = await refreshOpenAIQuota(accountID)
    if (props.account.id !== accountID || quotaSnapshotVersion !== snapshotVersion) return
    updateCredits(result)
    creditsCacheWarning.value = result.credits_cache_persisted === false
    // The upstream read succeeded even when the snapshot write was rejected, so
    // the live count is always adopted. Only the persisted view is left alone,
    // which keeps the displayed expirations consistent with what is stored.
    data.value = result
    if (result.cache_persisted) {
      cachedData.value = result
    } else {
      resetWarning.value = t('admin.accounts.openaiQuotaReset.refreshCachePersistFailed')
    }
  } catch (e) {
    if (props.account.id !== accountID || quotaSnapshotVersion !== snapshotVersion) return
    error.value = extractErrorMessage(e)
  } finally {
    if (props.account.id === accountID) loading.value = false
  }
}

// ---- 额度用尽后消耗点数开关 ----
const creditsEnabledOverride = ref<boolean | null>(null)
const creditsEnabled = computed(() =>
  creditsEnabledOverride.value ?? props.account.extra?.openai_credits_enabled === true
)
const creditsToggling = ref(false)
const showCreditsConfirm = ref(false)
const creditsToggleTitle = computed(() =>
  `${t('admin.accounts.openai.creditsEnabled')}: ${creditsEnabled.value ? t('common.enabled') : t('common.disabled')}\n${t('admin.accounts.openai.creditsEnabledDesc')}`
)

const applyCreditsEnabled = async (enabled: boolean) => {
  if (creditsToggling.value) return
  const accountID = props.account.id
  creditsToggling.value = true
  error.value = null
  try {
    const updated = await setOpenAICreditsEnabled(accountID, enabled)
    if (props.account.id !== accountID) return
    creditsEnabledOverride.value = enabled
    emit('account-updated', updated)
  } catch (e) {
    if (props.account.id !== accountID) return
    error.value = extractErrorMessage(e)
  } finally {
    if (props.account.id === accountID) creditsToggling.value = false
  }
}

const handleCreditsToggle = () => {
  if (creditsToggling.value) return
  if (creditsEnabled.value) {
    void applyCreditsEnabled(false)
    return
  }
  showCreditsConfirm.value = true
}

const confirmEnableCredits = () => {
  showCreditsConfirm.value = false
  void applyCreditsEnabled(true)
}

const applyAutoResetEnabled = async (enabled: boolean) => {
  if (autoResetToggling.value || resetting.value || !visible.value) return
  const accountID = props.account.id
  const requestVersion = ++autoResetToggleVersion
  autoResetToggling.value = true
  error.value = null
  try {
    const updated = await setOpenAIAutoResetCreditEnabled(accountID, enabled)
    if (props.account.id !== accountID || requestVersion !== autoResetToggleVersion) return
    autoResetEnabledOverride.value = updated.extra?.auto_reset_credit_enabled === true &&
      updated.extra?.auto_reset_credit_opt_in_version === 1
    emit('account-updated', updated)
  } catch (e) {
    if (props.account.id !== accountID || requestVersion !== autoResetToggleVersion) return
    error.value = extractErrorMessage(e)
  } finally {
    if (requestVersion === autoResetToggleVersion) autoResetToggling.value = false
  }
}

const handleAutoResetToggle = () => {
  if (autoResetToggling.value || resetting.value || !visible.value) return
  if (autoResetEnabled.value) {
    void applyAutoResetEnabled(false)
    return
  }
  showAutoResetConfirm.value = true
}

const confirmEnableAutoReset = () => {
  if (!showAutoResetConfirm.value) return
  showAutoResetConfirm.value = false
  void applyAutoResetEnabled(true)
}

const openResetConfirm = () => {
  if (resetting.value || loading.value) return
  if (manualResetBlockedReason.value) {
    error.value = t(manualResetBlockedReason.value)
    return
  }
  if (!canReset.value) {
    error.value = t('admin.accounts.openaiQuotaReset.noCreditsAvailable')
    return
  }
  showResetConfirm.value = true
}

const confirmReset = async () => {
  showResetConfirm.value = false
  if (resetting.value) return
  if (manualResetBlockedReason.value) {
    error.value = t(manualResetBlockedReason.value)
    return
  }
  if (!canReset.value) {
    error.value = t('admin.accounts.openaiQuotaReset.noCreditsAvailable')
    return
  }
  resetting.value = true
  const accountID = props.account.id
  creditsCacheWarning.value = false
  error.value = null
  resetMessage.value = null
  resetWarning.value = null
  try {
    const result: OpenAIQuotaResetResult = await resetOpenAIQuota(accountID)
    if (props.account.id !== accountID) return
    updateCredits(result.quota ?? null)
    showResetCreditDetails.value = false
    if (result.cache_refreshed && result.quota) {
      data.value = result.quota
      cachedData.value = result.quota
    } else {
      // A credit was consumed but the post-reset count could not be read back.
      // Whatever we still hold is one generation stale, so report the count as
      // unknown instead of letting a second consumption start from stale data.
      data.value = null
    }
    if (result.account) emit('account-updated', result.account)

    if (result.warning_code === 'reset_credit_cache_refresh_failed') {
      resetWarning.value = t('admin.accounts.openaiQuotaReset.resetCacheRefreshFailed')
    } else if (result.warning_code === 'account_state_recovery_failed') {
      resetWarning.value = t('admin.accounts.openaiQuotaReset.resetAccountRecoveryFailed')
    } else if (result.warning_code === 'account_state_refresh_failed') {
      resetWarning.value = t('admin.accounts.openaiQuotaReset.resetAccountRefreshFailed')
    } else {
      resetMessage.value = t('admin.accounts.openaiQuotaReset.resetSuccess', {
        windows: result.windows_reset
      })
    }
  } catch (e) {
    if (props.account.id !== accountID) return
    error.value = extractErrorMessage(e)
  } finally {
    if (props.account.id === accountID) resetting.value = false
  }
}

watch(
  [
    () => JSON.stringify(props.account.extra?.codex_reset_credit_snapshot ?? null),
    () => JSON.stringify(props.account.extra?.codex_credits_snapshot ?? null)
  ],
  ([resetSnapshot, creditsSnapshot], [previousResetSnapshot, previousCreditsSnapshot]) => {
    quotaSnapshotVersion++
    if (resetSnapshot !== previousResetSnapshot) {
      cachedData.value = readCachedResetCredits(props.account)
      data.value = cachedData.value
      showResetConfirm.value = false
      showResetCreditDetails.value = false
    }
    if (creditsSnapshot !== previousCreditsSnapshot) {
      creditsData.value = readCachedCredits(props.account)
      creditsCacheWarning.value = false
    }
  },
  { flush: 'sync' }
)

watch(
  () => props.account.id,
  () => {
    // Account row may be reused across paginated lists; reset local state.
    cachedData.value = readCachedResetCredits(props.account)
    data.value = cachedData.value
    creditsData.value = readCachedCredits(props.account)
    creditsCacheWarning.value = false
    error.value = null
    resetMessage.value = null
    resetWarning.value = null
    loading.value = false
    resetting.value = false
    showResetConfirm.value = false
    showResetCreditDetails.value = false
    creditsEnabledOverride.value = null
    creditsToggling.value = false
    showCreditsConfirm.value = false
  }
)

watch(
  () => [
    props.account.id,
    props.account.platform,
    props.account.type,
    props.account.extra?.auto_reset_credit_enabled,
    props.account.extra?.auto_reset_credit_opt_in_version,
    props.account.extra?.auto_reset_credit_5h_threshold,
    props.account.extra?.auto_reset_credit_7d_threshold
  ],
  (current, previous) => {
    autoResetEnabledOverride.value = null
    if (current.some((value, index) => value !== previous[index])) {
      autoResetToggleVersion++
      autoResetToggling.value = false
      showAutoResetConfirm.value = false
    }
  },
  { flush: 'sync' }
)
onBeforeUnmount(() => { autoResetToggleVersion++ })

// 父组件刷新账号后，以服务端状态为准
watch(
  () => props.account.extra?.openai_credits_enabled,
  () => {
    creditsEnabledOverride.value = null
  }
)

watch(
  resetCreditExpirations,
  () => {
    if (hiddenResetCreditCount.value <= 0) {
      showResetCreditDetails.value = false
    }
  }
)
</script>
