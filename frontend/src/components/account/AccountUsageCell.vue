<template>
  <div ref="rootRef" v-if="showUsageWindows">
    <!-- Anthropic OAuth and Setup Token accounts: fetch real usage data -->
    <template
      v-if="
        account.platform === 'anthropic' &&
        (account.type === 'oauth' || account.type === 'setup-token')
      "
    >
      <!-- Loading state -->
      <div v-if="loading" class="space-y-1.5">
        <!-- OAuth: 3 rows, Setup Token: 1 row -->
        <div class="flex items-center gap-1">
          <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
          <div class="h-1.5 w-8 animate-pulse rounded-full bg-gray-200 dark:bg-gray-700"></div>
          <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
        </div>
        <template v-if="account.type === 'oauth'">
          <div class="flex items-center gap-1">
            <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
            <div class="h-1.5 w-8 animate-pulse rounded-full bg-gray-200 dark:bg-gray-700"></div>
            <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
          </div>
          <div class="flex items-center gap-1">
            <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
            <div class="h-1.5 w-8 animate-pulse rounded-full bg-gray-200 dark:bg-gray-700"></div>
            <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
          </div>
        </template>
      </div>

      <!-- Error state -->
      <div v-else-if="error" class="text-xs text-red-500">
        {{ error }}
      </div>

      <!-- Usage data -->
      <div v-else-if="usageInfo" class="space-y-1">
        <!-- API error (degraded response) -->
        <div v-if="usageInfo.error" class="text-xs text-amber-600 dark:text-amber-400 truncate max-w-[200px]" :title="usageInfo.error">
          {{ usageInfo.error }}
        </div>
        <!-- 5h Window -->
        <UsageProgressBar
          v-if="usageInfo.five_hour"
          label="5h"
          :utilization="usageInfo.five_hour.utilization"
          :resets-at="usageInfo.five_hour.resets_at"
          :window-stats="usageInfo.five_hour.window_stats"
          color="indigo"
        />

        <!-- 7d Window (OAuth only) -->
        <UsageProgressBar
          v-if="usageInfo.seven_day"
          label="7d"
          :utilization="usageInfo.seven_day.utilization"
          :resets-at="usageInfo.seven_day.resets_at"
          color="emerald"
        />

        <!-- 7d Sonnet Window (OAuth only) -->
        <UsageProgressBar
          v-if="usageInfo.seven_day_sonnet"
          label="7d S"
          :utilization="usageInfo.seven_day_sonnet.utilization"
          :resets-at="usageInfo.seven_day_sonnet.resets_at"
          color="purple"
        />

        <!-- 7d Fable Window (7d_oi) -->
        <UsageProgressBar
          v-if="usageInfo.seven_day_fable"
          label="7d F"
          :utilization="usageInfo.seven_day_fable.utilization"
          :resets-at="usageInfo.seven_day_fable.resets_at"
          color="amber"
        />

        <!-- Passive sampling label + active query button -->
        <div class="flex items-center gap-1.5 mt-0.5">
          <span
            v-if="usageInfo.source === 'passive'"
            class="text-[9px] text-gray-400 dark:text-gray-500 italic"
          >
            {{ t('admin.accounts.usageWindow.passiveSampled') }}
          </span>
          <button
            type="button"
            class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[9px] font-medium text-blue-600 hover:bg-blue-50 dark:text-blue-400 dark:hover:bg-blue-900/30 transition-colors"
            :disabled="activeQueryLoading"
            @click="loadActiveUsage"
          >
            <svg
              class="h-2.5 w-2.5"
              :class="{ 'animate-spin': activeQueryLoading }"
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
            {{ t('admin.accounts.usageWindow.activeQuery') }}
          </button>
        </div>
      </div>

      <!-- No data yet -->
      <div v-else class="space-y-1">
        <div class="text-xs text-gray-400">-</div>
      </div>
    </template>

    <!-- OpenAI OAuth accounts: single source from /usage API -->
    <template v-else-if="account.platform === 'openai' && account.type === 'oauth'">
      <div v-if="hasOpenAIUsageFallback" class="space-y-1">
        <UsageProgressBar
          v-if="usageInfo?.five_hour"
          label="5h"
          :utilization="usageInfo.five_hour.utilization"
          :resets-at="usageInfo.five_hour.resets_at"
          :window-stats="usageInfo.five_hour.window_stats"
          :show-now-when-idle="true"
          color="indigo"
        />
        <UsageProgressBar
          v-if="usageInfo?.seven_day"
          label="7d"
          :utilization="usageInfo.seven_day.utilization"
          :resets-at="usageInfo.seven_day.resets_at"
          :window-stats="usageInfo.seven_day.window_stats"
          :show-now-when-idle="true"
          color="emerald"
        />
        <!--
          Upstream codex /wham/usage quota query + reset. The local active-sampling
          refresh button is rendered via the pre-actions slot so the user sees a
          single row of related buttons instead of two stacked rows.
        -->
        <OpenAIQuotaResetCell :account="account" @account-updated="handleQuotaResetAccountUpdated">
          <template #pre-actions>
            <button
              type="button"
              class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium text-blue-600 hover:bg-blue-50 dark:text-blue-400 dark:hover:bg-blue-900/30 transition-colors disabled:cursor-not-allowed disabled:opacity-50"
              :disabled="activeQueryLoading"
              @click="loadActiveUsage"
            >
              <svg
                class="h-2.5 w-2.5"
                :class="{ 'animate-spin': activeQueryLoading }"
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
              {{ t('admin.accounts.usageWindow.activeQuery') }}
            </button>
          </template>
        </OpenAIQuotaResetCell>
      </div>
      <div v-else-if="loading" class="space-y-1.5">
        <div class="flex items-center gap-1">
          <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
          <div class="h-1.5 w-8 animate-pulse rounded-full bg-gray-200 dark:bg-gray-700"></div>
          <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
        </div>
        <div class="flex items-center gap-1">
          <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
          <div class="h-1.5 w-8 animate-pulse rounded-full bg-gray-200 dark:bg-gray-700"></div>
          <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
        </div>
      </div>
      <div v-else>
        <div class="text-xs text-gray-400">-</div>
        <!-- Always allow on-demand upstream quota query, even before local data exists. -->
        <OpenAIQuotaResetCell
          :account="account"
          class="mt-1"
          @account-updated="handleQuotaResetAccountUpdated"
        />
      </div>
    </template>

    <!-- Grok OAuth accounts: passive xAI quota headers + local Sub2API usage -->
    <template v-else-if="account.platform === 'grok' && account.type === 'oauth'">
      <div v-if="loading" class="space-y-1.5">
        <div class="flex items-center gap-1">
          <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
          <div class="h-1.5 w-8 animate-pulse rounded-full bg-gray-200 dark:bg-gray-700"></div>
          <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
        </div>
      </div>
      <div v-else-if="error" class="text-xs text-red-500">
        {{ error }}
      </div>
      <div v-else-if="needsReauth" class="space-y-1">
        <span class="inline-block rounded px-1.5 py-0.5 text-[10px] font-medium bg-orange-100 text-orange-700 dark:bg-orange-900/40 dark:text-orange-300">
          {{ t('admin.accounts.needsReauth') }}
        </span>
      </div>
      <div v-else-if="isForbidden" class="space-y-1">
        <span class="inline-block rounded px-1.5 py-0.5 text-[10px] font-medium bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300">
          {{ usageInfo?.grok_entitlement_status || t('admin.accounts.forbidden') }}
        </span>
      </div>
      <div v-else-if="usageInfo" class="space-y-1">
        <!-- Free: only rolling 24h soft-gate bar. Paid: 7d + 30d + prepaid money. -->
        <template v-if="grokIsFree">
          <UsageProgressBar
            v-if="grokFreeTokenBar"
            label="24h"
            :title="t('admin.accounts.usageWindow.grokFreeQuota24hHint', { limit: formatCompactNumber(grokFreeTokenBar.limit) })"
            :utilization="grokFreeTokenBar.utilization"
            :window-stats="grokFreeQuotaUsage"
            :show-now-when-idle="true"
            color="emerald"
          />
          <div v-else-if="grokQuotaUnknown" class="text-[10px] text-gray-500 dark:text-gray-400">
            {{ grokQuotaUnknownLabel }}
          </div>
        </template>
        <template v-else>
          <UsageProgressBar
            v-if="grokWeeklyBillingBar"
            label="7d"
            :utilization="grokWeeklyBillingBar.utilization"
            :resets-at="grokWeeklyBillingBar.resetsAt"
            :window-stats="grokWeeklyBillingBar.windowStats"
            :show-now-when-idle="true"
            color="indigo"
          />
          <UsageProgressBar
            v-if="grokMonthlyBillingBar"
            label="30d"
            :utilization="grokMonthlyBillingBar.utilization"
            :resets-at="grokMonthlyBillingBar.resetsAt"
            :window-stats="grokMonthlyBillingBar.windowStats"
            :show-now-when-idle="true"
            color="indigo"
          />
          <div
            v-if="grokPrepaidMoneyLine"
            class="flex flex-wrap items-center gap-1 text-[10px] text-gray-500 dark:text-gray-400"
          >
            <span
              v-if="grokPrepaidMoneyLine.showPrepaid"
              class="rounded bg-emerald-50 px-1 py-0.5 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300"
              :title="t('admin.accounts.usageWindow.grokPrepaid')"
            >
              {{ t('admin.accounts.usageWindow.grokPrepaid') }} ${{ grokPrepaidMoneyLine.prepaid }}
            </span>
            <span
              v-if="grokPrepaidMoneyLine.showUsedLimit"
              :title="t('admin.accounts.usageWindow.grokMonthlyLimit')"
            >
              {{ t('admin.accounts.usageWindow.grokUsed') }}
              {{ grokPrepaidMoneyLine.used }}/{{ grokPrepaidMoneyLine.limit }}
            </span>
          </div>
          <div v-if="grokQuotaUnknown" class="text-[10px] text-gray-500 dark:text-gray-400">
            {{ grokQuotaUnknownLabel }}
          </div>
        </template>
        <div v-if="usageInfo.error" class="truncate text-xs text-amber-600 dark:text-amber-400 max-w-[200px]" :title="usageInfo.error">
          {{ usageErrorLabel }}
        </div>
        <div v-if="grokRetryAfterLabel" class="text-[10px] text-amber-600 dark:text-amber-400">
          {{ t('admin.accounts.usageWindow.grokRetryAfter', { time: grokRetryAfterLabel }) }}
        </div>
        <GrokQuotaProbeCell :account="account" compact @probed="handleGrokProbed" />
      </div>
      <div v-else class="space-y-1">
        <div class="text-xs text-gray-400">-</div>
        <GrokQuotaProbeCell :account="account" compact @probed="handleGrokProbed" />
      </div>
    </template>

    <!-- CN providers (Kimi / Zhipu / DeepSeek): coding-plan quota or payg balance -->
    <template v-else-if="account.platform === 'kimi' || account.platform === 'zhipu' || account.platform === 'deepseek'">
      <!-- 挂在 CN 平台下的 Ollama Cloud 账号（资格由后端下发 eligible）：用量由
           Ollama 用量窗口负责。这类账号不是国产厂商订阅，CN 的额度/余额探测端点由
           base_url 衍生，对 ollama.com 会被后端出站 URL 白名单拒绝，渲染出来只会
           给用户一行探测报错，因此不再渲染 CN 子单元格与占位符。 -->
      <OllamaCloudUsageCell
        v-if="account.ollama_cloud_usage?.eligible"
        :account="account"
        @updated="handleOllamaCloudUsageUpdated"
      />
      <div v-else class="space-y-1">
        <!-- 子单元格各自按 模式×平台 判定可见；两者都不可见时（智谱 payg 无公开
             余额端点、coding 探测也不适用）才回落到占位符。 -->
        <div
          v-if="!cnQuotaCellVisible && !cnBalanceCellVisible"
          class="text-xs text-gray-400"
          :title="t('admin.accounts.cnProviders.noBalanceEndpoint')"
        >-</div>
        <CNProviderQuotaCell :account="account" />
        <CNProviderBalanceCell :account="account" />
      </div>
    </template>

    <!-- Other accounts: no usage window -->
    <template v-else>
      <div class="text-xs text-gray-400">-</div>
    </template>
  </div>

  <!-- Non-OAuth/Setup-Token accounts -->
  <div ref="rootRef" v-else>
    <!-- Key/Bedrock accounts: show today stats + optional quota bars -->
    <div class="space-y-1">
      <OllamaCloudUsageCell
        v-if="account.ollama_cloud_usage?.eligible"
        :account="account"
        @updated="handleOllamaCloudUsageUpdated"
      />
      <!-- Today stats row (requests, tokens, cost, user_cost) -->
      <div
        v-if="todayStats"
        class="mb-0.5 flex items-center"
      >
        <div class="flex items-center gap-1.5 text-[9px] text-gray-500 dark:text-gray-400">
          <span class="rounded bg-gray-100 px-1.5 py-0.5 dark:bg-gray-800">
            {{ formatKeyRequests }} req
          </span>
          <span class="rounded bg-gray-100 px-1.5 py-0.5 dark:bg-gray-800">
            {{ formatKeyTokens }}
          </span>
          <span class="rounded bg-gray-100 px-1.5 py-0.5 dark:bg-gray-800" :title="t('usage.accountBilled')">
            A ${{ formatKeyCost }}
          </span>
          <span
            v-if="todayStats.user_cost != null"
            class="rounded bg-gray-100 px-1.5 py-0.5 dark:bg-gray-800"
            :title="t('usage.userBilled')"
          >
            U ${{ formatKeyUserCost }}
          </span>
        </div>
      </div>
      <!-- Loading skeleton for today stats -->
      <div
        v-else-if="todayStatsLoading"
        class="mb-0.5 flex items-center gap-1"
      >
        <div class="h-3 w-10 animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
        <div class="h-3 w-8 animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
        <div class="h-3 w-12 animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
      </div>

      <!-- API Key accounts with quota limits: show progress bars -->
      <UsageProgressBar
        v-if="quotaDailyBar"
        label="1d"
        :utilization="quotaDailyBar.utilization"
        :resets-at="quotaDailyBar.resetsAt"
        color="indigo"
      />
      <UsageProgressBar
        v-if="quotaWeeklyBar"
        label="7d"
        :utilization="quotaWeeklyBar.utilization"
        :resets-at="quotaWeeklyBar.resetsAt"
        color="emerald"
      />
      <UsageProgressBar
        v-if="quotaTotalBar"
        label="total"
        :utilization="quotaTotalBar.utilization"
        color="purple"
      />

      <!-- No data at all -->
      <div
        v-if="!todayStats && !todayStatsLoading && !hasApiKeyQuota && !account.ollama_cloud_usage?.eligible"
        class="text-xs text-gray-400"
      >-</div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount, onUnmounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { Account, AccountUsageInfo, WindowStats } from '@/types'
import { buildOpenAIUsageRefreshKey } from '@/utils/accountUsageRefresh'
import { enqueueUsageRequest } from '@/utils/usageLoadQueue'
import { formatCompactNumber } from '@/utils/format'
import UsageProgressBar from './UsageProgressBar.vue'
import OpenAIQuotaResetCell from './OpenAIQuotaResetCell.vue'
import GrokQuotaProbeCell from './GrokQuotaProbeCell.vue'
import CNProviderQuotaCell from './CNProviderQuotaCell.vue'
import CNProviderBalanceCell from './CNProviderBalanceCell.vue'
import OllamaCloudUsageCell from './OllamaCloudUsageCell.vue'
import { cnQuotaCellVisible as cnQuotaCellVisibleFn, cnBalanceCellVisible as cnBalanceCellVisibleFn } from './credentialsBuilder'

// Module-level cache shared across all AccountUsageCell instances
const _usageCache = new Map<number, { data: AccountUsageInfo; ts: number }>()
const USAGE_CACHE_TTL = 5 * 60 * 1000 // 5 minutes

const props = withDefaults(
  defineProps<{
    account: Account
    todayStats?: WindowStats | null
    todayStatsLoading?: boolean
    manualRefreshToken?: number
    batchedUsage?: AccountUsageInfo | null
    batchedUsageError?: string | null
    batchedUsageLoading?: boolean
    requestBatchedUsage?: ((account: Account, options?: { force?: boolean }) => void) | null
  }>(),
  {
    todayStats: null,
    todayStatsLoading: false,
    manualRefreshToken: 0,
    batchedUsage: null,
    batchedUsageError: null,
    batchedUsageLoading: false,
    requestBatchedUsage: null
  }
)

const emit = defineEmits<{
  'account-updated': [account: Account]
  'usage-loaded': [usage: AccountUsageInfo]
}>()

const { t } = useI18n()
const desktopViewportQuery = '(min-width: 768px)'

const unmounted = ref(false)
onBeforeUnmount(() => { unmounted.value = true })

const loading = ref(false)
const activeQueryLoading = ref(false)
const error = ref<string | null>(null)
const usageInfo = ref<AccountUsageInfo | null>(null)
watch(usageInfo, (usage) => {
  if (usage) emit('usage-loaded', usage)
})
const rootRef = ref<HTMLElement | null>(null)
const isDesktopViewport = ref(
  typeof window === 'undefined' ? true : window.matchMedia(desktopViewportQuery).matches
)
const hasEnteredViewport = ref(false)
const pendingAutoLoad = ref(false)
const pendingAutoLoadSource = ref<'passive' | 'active' | undefined>(undefined)

let desktopViewportMediaQuery: MediaQueryList | null = null
let desktopViewportListener: ((event: MediaQueryListEvent) => void) | null = null
let visibilityObserver: IntersectionObserver | null = null

// Show usage windows for OAuth and Setup Token accounts
const showUsageWindows = computed(() => {
  // CN providers: apikey 账号也有滚动用量窗口（coding plan）或余额（payg），
  // 由 CNProviderQuotaCell / CNProviderBalanceCell 自行探测与展示。
  if (
    props.account.platform === 'kimi' ||
    props.account.platform === 'zhipu' ||
    props.account.platform === 'deepseek'
  ) {
    return true
  }
  return props.account.type === 'oauth' || props.account.type === 'setup-token'
})

const shouldFetchUsage = computed(() => {
  if (props.account.platform === 'anthropic') {
    return props.account.type === 'oauth' || props.account.type === 'setup-token'
  }
  if (props.account.platform === 'grok') {
    return props.account.type === 'oauth'
  }
  if (props.account.platform === 'openai') {
    return props.account.type === 'oauth'
  }
  return false
})

// CN 供应商子单元格可见性（与 CNProviderQuotaCell / CNProviderBalanceCell 共用
// credentialsBuilder 的单一实现）：都不可见时显示 `-` 占位符。
const cnAccountMode = computed(() => {
  const mode = props.account.credentials?.account_mode
  return typeof mode === 'string' ? mode : ''
})
const cnQuotaCellVisible = computed(() => cnQuotaCellVisibleFn(props.account.platform, cnAccountMode.value))
const cnBalanceCellVisible = computed(() => cnBalanceCellVisibleFn(props.account.platform, cnAccountMode.value))

const isBatchManaged = computed(() => typeof props.requestBatchedUsage === 'function')

const hasOpenAIUsageFallback = computed(() => {
  if (props.account.platform !== 'openai' || props.account.type !== 'oauth') return false
  return !!usageInfo.value?.five_hour || !!usageInfo.value?.seven_day
})

const openAIUsageRefreshKey = computed(() => buildOpenAIUsageRefreshKey(props.account))

const shouldAutoLoadUsageOnMount = computed(() => {
  return shouldFetchUsage.value
})

const shouldLazyLoadOnMobile = computed(() => {
  return shouldFetchUsage.value && !isDesktopViewport.value
})

interface GrokQuotaBarInfo {
  utilization: number
  resetsAt: string | null
  windowStats?: WindowStats | null
}

const grokBilling = computed(() => usageInfo.value?.grok_billing || null)
const grokLocalUsage7d = computed(() => (
  usageInfo.value?.grok_local_usage_7d || usageInfo.value?.seven_day?.window_stats || null
))
const grokLocalUsageMonthly = computed(() => (
  usageInfo.value?.grok_local_usage_monthly || usageInfo.value?.thirty_day?.window_stats || null
))
const grokWeeklyBillingBar = computed((): GrokQuotaBarInfo | null => {
  const billing = grokBilling.value
  if (billing?.period_type?.toLowerCase() !== 'weekly' || billing.usage_percent == null) {
    return null
  }
  return {
    utilization: Math.min(100, Math.max(0, billing.usage_percent)),
    resetsAt: billing.period_end || null,
    windowStats: grokLocalUsage7d.value
  }
})
// Monthly used/limit % from billing probe (used_percent or derived from cents).
const grokMonthlyBillingBar = computed((): GrokQuotaBarInfo | null => {
  const billing = grokBilling.value
  if (!billing) return null
  let utilization: number | null = null
  if (billing.used_percent != null && Number.isFinite(billing.used_percent)) {
    utilization = billing.used_percent
  } else if (
    billing.monthly_limit_cents != null &&
    billing.monthly_limit_cents > 0 &&
    billing.used_cents != null
  ) {
    utilization = (billing.used_cents / billing.monthly_limit_cents) * 100
  }
  if (utilization == null) return null
  // Avoid duplicating the weekly bar when period_type is weekly-only without monthly.
  if (billing.period_type?.toLowerCase() === 'weekly' && billing.monthly_limit_cents == null) {
    return null
  }
  return {
    utilization: Math.min(100, Math.max(0, utilization)),
    resetsAt: billing.billing_period_end || billing.period_end || null,
    windowStats: grokLocalUsageMonthly.value
  }
})
const formatGrokMoney = (value?: number | null) => {
  if (value == null || Number.isNaN(value)) return '0'
  if (value >= 1000) return formatCompactNumber(value)
  if (value >= 100) return value.toFixed(0)
  if (value >= 10) return value.toFixed(1)
  return value.toFixed(2)
}
// Prepaid chip only when there is a positive prepaid balance.
// Used/limit only when monthly limit is a positive number (0 means unlimited / unset).
const grokPrepaidMoneyLine = computed(() => {
  const billing = grokBilling.value
  if (!billing) return null
  const prepaid = billing.prepaid_balance
  const showPrepaid = prepaid != null && Number.isFinite(prepaid) && prepaid > 0
  const limitRaw =
    billing.monthly_limit != null
      ? billing.monthly_limit
      : billing.monthly_limit_cents != null
        ? billing.monthly_limit_cents / 100
        : null
  const showUsedLimit = limitRaw != null && Number.isFinite(limitRaw) && limitRaw > 0
  if (!showPrepaid && !showUsedLimit) return null
  const used =
    billing.monthly_used != null
      ? billing.monthly_used
      : billing.used_cents != null
        ? billing.used_cents / 100
        : 0
  return {
    showPrepaid,
    showUsedLimit,
    prepaid: showPrepaid ? formatGrokMoney(prepaid) : null,
    used: showUsedLimit ? formatGrokMoney(used) : null,
    limit: showUsedLimit ? formatGrokMoney(limitRaw) : null
  }
})
const grokPlanLabelIsFree = (value: string) => value.includes('free') || value.includes('basic')
const grokPlanLabelIsPaid = (value: string) => {
  return value !== '' && !grokPlanLabelIsFree(value) && !value.includes('unknown')
}
const grokIsFree = computed(() => {
  if (props.account.platform !== 'grok' || props.account.type !== 'oauth') return false
  const billing = grokBilling.value
  const plan = (billing?.plan || '').trim().toLowerCase()
  const tier = (usageInfo.value?.subscription_tier || '').trim().toLowerCase()
  const entitlement = (usageInfo.value?.grok_entitlement_status || '').toLowerCase()
  if (grokPlanLabelIsFree(tier)) return true
  if (grokPlanLabelIsPaid(tier)) return false
  if (
    billing?.usage_percent != null ||
    billing?.used_percent != null ||
    (billing?.monthly_limit_cents != null && billing.monthly_limit_cents > 0)
  ) return false
  if (grokPlanLabelIsPaid(plan)) return false
  if (
    grokPlanLabelIsFree(plan) ||
    grokPlanLabelIsFree(entitlement)
  ) return true
  return billing != null
})
const grokFreeQuotaUsage = computed(() => usageInfo.value?.grok_local_usage_24h || null)
const grokFreeTokenBar = computed(() => {
  if (!grokIsFree.value || !grokFreeQuotaUsage.value) return null
  const limit = usageInfo.value?.grok_free_token_limit
  if (typeof limit !== 'number' || limit <= 0) return null
  const used = Math.max(0, grokFreeQuotaUsage.value.tokens || 0)
  return { utilization: Math.min(100, (used / limit) * 100), limit }
})
const grokQuotaUnknown = computed(() => {
  if (props.account.platform !== 'grok') return false
  if (grokIsFree.value) {
    return !grokFreeTokenBar.value
  }
  if (grokWeeklyBillingBar.value || grokMonthlyBillingBar.value || grokPrepaidMoneyLine.value) {
    return false
  }
  return usageInfo.value?.grok_quota_snapshot_state !== 'observed'
})
const grokQuotaUnknownLabel = computed(() => {
  return usageInfo.value?.grok_quota_snapshot_state === 'no_headers'
    ? t('admin.accounts.usageWindow.grokNoHeaders')
    : t('admin.accounts.usageWindow.grokUnknown')
})
const grokRetryAfterLabel = computed(() => {
  const seconds = usageInfo.value?.grok_retry_after_seconds
  if (seconds == null || seconds <= 0) return null
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.ceil(seconds / 60)
  return `${minutes}m`
})

// 403 forbidden 状态
const isForbidden = computed(() => !!usageInfo.value?.is_forbidden)
const forbiddenType = computed(() => usageInfo.value?.forbidden_type || 'forbidden')
const validationURL = computed(() => usageInfo.value?.validation_url || '')

// 需要重新授权（401）
const needsReauth = computed(() => !!usageInfo.value?.needs_reauth)

// 降级错误标签（rate_limited / network_error）
const usageErrorLabel = computed(() => {
  const code = usageInfo.value?.error_code
  if (code === 'rate_limited') return t('admin.accounts.rateLimited')
  return t('admin.accounts.usageError')
})

const forbiddenLabel = computed(() => {
  switch (forbiddenType.value) {
    case 'validation':
      return t('admin.accounts.forbiddenValidation')
    case 'violation':
      return t('admin.accounts.forbiddenViolation')
    default:
      return t('admin.accounts.forbidden')
  }
})

const forbiddenBadgeClass = computed(() => {
  if (forbiddenType.value === 'validation') {
    return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/40 dark:text-yellow-300'
  }
  return 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'
})

const linkCopied = ref(false)
const copyValidationURL = async () => {
  if (!validationURL.value) return
  try {
    await navigator.clipboard.writeText(validationURL.value)
    linkCopied.value = true
    setTimeout(() => { linkCopied.value = false }, 2000)
  } catch {
    // fallback: ignore
  }
}

const isAnthropicOAuthOrSetupToken = computed(() => {
  return props.account.platform === 'anthropic' && (props.account.type === 'oauth' || props.account.type === 'setup-token')
})

const requestParentBatchUsage = (options?: { force?: boolean }) => {
  if (!isBatchManaged.value || !shouldFetchUsage.value) return
  props.requestBatchedUsage?.(props.account, options)
}

const syncManagedUsageState = () => {
  if (!isBatchManaged.value) return
  usageInfo.value = props.batchedUsage ?? null
  error.value = props.batchedUsageError ?? null
  loading.value = props.batchedUsageLoading === true
}

const loadUsage = async (options?: { source?: 'passive' | 'active'; bypassCache?: boolean }) => {
  if (!shouldFetchUsage.value) return
  if (isBatchManaged.value) {
    requestParentBatchUsage({ force: options?.bypassCache === true })
    return
  }

  // Check cache
  if (!options?.bypassCache) {
    const cached = _usageCache.get(props.account.id)
    if (cached && Date.now() - cached.ts < USAGE_CACHE_TTL) {
      usageInfo.value = cached.data
      loading.value = false
      return
    }
  }

  loading.value = true
  error.value = null

  try {
		const fetchFn = () => options?.source
			? adminAPI.accounts.getUsage(props.account.id, options.source, options.bypassCache === true)
			: adminAPI.accounts.getUsage(props.account.id)
    const result = await enqueueUsageRequest(props.account, fetchFn)
    if (!unmounted.value) {
      usageInfo.value = result
      _usageCache.set(props.account.id, { data: result, ts: Date.now() })
    }
  } catch (e: any) {
    if (!unmounted.value) {
      error.value = t('common.error')
      console.error('Failed to load usage:', e)
    }
  } finally {
    if (!unmounted.value) loading.value = false
  }
}

const flushPendingAutoLoad = () => {
  if (!pendingAutoLoad.value) return
  const source = pendingAutoLoadSource.value
  pendingAutoLoad.value = false
  pendingAutoLoadSource.value = undefined
  loadUsage({ source }).catch((e) => {
    console.error('Failed to load deferred usage:', e)
  })
}

const requestAutoLoad = (source?: 'passive' | 'active') => {
  if (!shouldFetchUsage.value) return
  if (shouldLazyLoadOnMobile.value && !hasEnteredViewport.value) {
    pendingAutoLoad.value = true
    pendingAutoLoadSource.value = source
    return
  }
  loadUsage({ source }).catch((e) => {
    console.error('Failed to auto load usage:', e)
  })
}

const detachVisibilityObserver = () => {
  visibilityObserver?.disconnect()
  visibilityObserver = null
}

const attachVisibilityObserver = () => {
  detachVisibilityObserver()
  if (!shouldLazyLoadOnMobile.value || hasEnteredViewport.value) return
  if (typeof window === 'undefined' || typeof IntersectionObserver === 'undefined') {
    hasEnteredViewport.value = true
    flushPendingAutoLoad()
    return
  }
  if (!rootRef.value) return

  visibilityObserver = new IntersectionObserver((entries) => {
    if (!entries.some((entry) => entry.isIntersecting)) return
    hasEnteredViewport.value = true
    detachVisibilityObserver()
    flushPendingAutoLoad()
  }, {
    root: null,
    rootMargin: '200px 0px',
    threshold: 0.01
  })
  visibilityObserver.observe(rootRef.value)
}

const loadActiveUsage = async () => {
  activeQueryLoading.value = true
  try {
    usageInfo.value = await adminAPI.accounts.getUsage(props.account.id, 'active', true)
  } catch (e: any) {
    console.error('Failed to load active usage:', e)
  } finally {
    activeQueryLoading.value = false
  }
}

// The probe persists upstream quota state; refresh this cell so its compact
// bars and entitlement status reflect the newly observed snapshot.
const handleGrokProbed = async () => {
  await loadUsage({ source: 'active', bypassCache: true })
}

// ===== API Key quota progress bars =====

interface QuotaBarInfo {
  utilization: number
  resetsAt: string | null
}

const makeQuotaBar = (
  used: number,
  limit: number,
  startKey?: string
): QuotaBarInfo => {
  const utilization = limit > 0 ? (used / limit) * 100 : 0
  let resetsAt: string | null = null
  if (startKey) {
    const extra = props.account.extra as Record<string, unknown> | undefined
    const isDaily = startKey.includes('daily')
    const mode = isDaily
      ? (extra?.quota_daily_reset_mode as string) || 'rolling'
      : (extra?.quota_weekly_reset_mode as string) || 'rolling'

    if (mode === 'fixed') {
      // Use pre-computed next reset time for fixed mode
      const resetAtKey = isDaily ? 'quota_daily_reset_at' : 'quota_weekly_reset_at'
      resetsAt = (extra?.[resetAtKey] as string) || null
    } else {
      // Rolling mode: compute from start + period
      const startStr = extra?.[startKey] as string | undefined
      if (startStr) {
        const startDate = new Date(startStr)
        const periodMs = isDaily ? 24 * 60 * 60 * 1000 : 7 * 24 * 60 * 60 * 1000
        resetsAt = new Date(startDate.getTime() + periodMs).toISOString()
      }
    }
  }
  return { utilization, resetsAt }
}

const hasApiKeyQuota = computed(() => {
  if (props.account.type !== 'apikey' && props.account.type !== 'bedrock') return false
  return (
    (props.account.quota_daily_limit ?? 0) > 0 ||
    (props.account.quota_weekly_limit ?? 0) > 0 ||
    (props.account.quota_limit ?? 0) > 0
  )
})

const quotaDailyBar = computed((): QuotaBarInfo | null => {
  const limit = props.account.quota_daily_limit ?? 0
  if (limit <= 0) return null
  return makeQuotaBar(props.account.quota_daily_used ?? 0, limit, 'quota_daily_start')
})

const quotaWeeklyBar = computed((): QuotaBarInfo | null => {
  const limit = props.account.quota_weekly_limit ?? 0
  if (limit <= 0) return null
  return makeQuotaBar(props.account.quota_weekly_used ?? 0, limit, 'quota_weekly_start')
})

const quotaTotalBar = computed((): QuotaBarInfo | null => {
  const limit = props.account.quota_limit ?? 0
  if (limit <= 0) return null
  return makeQuotaBar(props.account.quota_used ?? 0, limit)
})

const handleQuotaResetAccountUpdated = (account: Account) => {
  emit('account-updated', account)
}

const handleOllamaCloudUsageUpdated = (state: NonNullable<Account['ollama_cloud_usage']>) => {
  emit('account-updated', { ...props.account, ollama_cloud_usage: state })
}

// ===== Key account today stats formatters =====

const formatKeyRequests = computed(() => {
  if (!props.todayStats) return ''
  return formatCompactNumber(props.todayStats.requests, { allowBillions: false })
})

const formatKeyTokens = computed(() => {
  if (!props.todayStats) return ''
  return formatCompactNumber(props.todayStats.tokens)
})

const formatKeyCost = computed(() => {
  if (!props.todayStats) return '0.00'
  return props.todayStats.cost.toFixed(2)
})

const formatKeyUserCost = computed(() => {
  if (!props.todayStats || props.todayStats.user_cost == null) return '0.00'
  return props.todayStats.user_cost.toFixed(2)
})

onMounted(() => {
  if (typeof window !== 'undefined') {
    desktopViewportMediaQuery = window.matchMedia(desktopViewportQuery)
    isDesktopViewport.value = desktopViewportMediaQuery.matches
    desktopViewportListener = (event: MediaQueryListEvent) => {
      isDesktopViewport.value = event.matches
    }
    if (typeof desktopViewportMediaQuery.addEventListener === 'function') {
      desktopViewportMediaQuery.addEventListener('change', desktopViewportListener)
    } else {
      desktopViewportMediaQuery.addListener(desktopViewportListener)
    }
  }

  if (isBatchManaged.value) {
    syncManagedUsageState()
    requestParentBatchUsage()
    return
  }

  if (!shouldAutoLoadUsageOnMount.value) return
  const source = isAnthropicOAuthOrSetupToken.value ? 'passive' : undefined
  requestAutoLoad(source)
})

watch(
  () => [props.batchedUsage, props.batchedUsageError, props.batchedUsageLoading, isBatchManaged.value] as const,
  () => {
    syncManagedUsageState()
  },
  { immediate: true, deep: true }
)

watch(isBatchManaged, (managed, wasManaged) => {
  if (managed && !wasManaged) {
    syncManagedUsageState()
    requestParentBatchUsage()
  }
})

watch(
  () => [props.account.id, props.account.platform, props.account.type, isBatchManaged.value] as const,
  ([accountID, platform, accountType, managed], [previousAccountID, previousPlatform, previousAccountType]) => {
    if (
      accountID === previousAccountID &&
      platform === previousPlatform &&
      accountType === previousAccountType
    ) {
      return
    }
    if (!managed || !shouldFetchUsage.value) return
    syncManagedUsageState()
    requestParentBatchUsage()
  },
  { flush: 'post' }
)

watch(openAIUsageRefreshKey, (nextKey, prevKey) => {
  if (!prevKey || nextKey === prevKey) return
  if (props.account.platform !== 'openai' || props.account.type !== 'oauth') return

  if (isBatchManaged.value) {
    requestParentBatchUsage({ force: true })
    return
  }

  _usageCache.delete(props.account.id)
  requestAutoLoad()
})

watch(
  () => props.manualRefreshToken,
  (nextToken, prevToken) => {
    if (nextToken === prevToken) return
    if (!shouldFetchUsage.value) return

    if (isBatchManaged.value) {
      requestParentBatchUsage({ force: true })
      return
    }

    const source = isAnthropicOAuthOrSetupToken.value ? 'passive' : undefined
    _usageCache.delete(props.account.id)
    loadUsage({ source, bypassCache: true }).catch((e) => {
      console.error('Failed to refresh usage after manual refresh:', e)
    })
  }
)

watch(
  [rootRef, shouldLazyLoadOnMobile],
  () => {
    if (shouldLazyLoadOnMobile.value) {
      attachVisibilityObserver()
      return
    }
    detachVisibilityObserver()
  },
  { immediate: true, flush: 'post' }
)

watch(isDesktopViewport, (isDesktop) => {
  if (isDesktop) {
    detachVisibilityObserver()
    hasEnteredViewport.value = true
    flushPendingAutoLoad()
    return
  }
  hasEnteredViewport.value = false
  attachVisibilityObserver()
})

onUnmounted(() => {
  detachVisibilityObserver()
  if (desktopViewportMediaQuery && desktopViewportListener) {
    if (typeof desktopViewportMediaQuery.removeEventListener === 'function') {
      desktopViewportMediaQuery.removeEventListener('change', desktopViewportListener)
    } else {
      desktopViewportMediaQuery.removeListener(desktopViewportListener)
    }
  }
  desktopViewportListener = null
  desktopViewportMediaQuery = null
})
</script>
