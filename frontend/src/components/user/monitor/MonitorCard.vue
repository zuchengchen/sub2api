<template>
  <button
    type="button"
    class="group text-left p-5 rounded-2xl min-h-[280px] w-full bg-white/70 backdrop-blur-xl border border-gray-200/80 shadow-card dark:bg-dark-800/60 dark:border-dark-700/70 hover:-translate-y-1 hover:shadow-card-hover dark:hover:border-primary-500/30 hover:border-gray-300 transition-all duration-300 ease-out flex flex-col"
    @click="emit('click')"
  >
    <!-- Header: icon + name / group+rate + status chip -->
    <div class="flex items-start gap-3">
      <span
        class="w-9 h-9 rounded-xl ring-1 ring-black/5 dark:ring-white/10 grid place-items-center flex-shrink-0"
        :class="[providerGradient(item.provider), providerTintClass]"
      >
        <ProviderIcon :provider="item.provider" :size="20" />
      </span>
      <div class="flex-1 min-w-0">
        <div class="text-base font-semibold truncate text-gray-900 dark:text-gray-100">
          {{ item.name }}
        </div>
        <div
          v-if="item.group_name || hasRate"
          class="mt-0.5 flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5"
        >
          <span
            v-if="item.group_name"
            class="min-w-0 truncate text-xs text-gray-500 dark:text-gray-400"
            :title="item.group_name"
          >{{ item.group_name }}</span>
          <span
            v-if="hasCustomRate"
            class="shrink-0 text-xs tabular-nums text-gray-400 line-through"
          >{{ formattedDefaultRate }}</span>
          <span
            v-if="hasRate"
            data-testid="monitor-card-rate"
            class="shrink-0 text-lg font-extrabold leading-none tabular-nums text-red-600 dark:text-red-400"
          >{{ formattedEffectiveRate }}</span>
          <span
            v-if="hasPeakRate"
            class="min-w-0 text-[11px] font-medium text-amber-600 dark:text-amber-400"
            :title="peakRateTitle"
          >{{ peakRateText }}</span>
        </div>
      </div>
      <span
        class="px-2.5 py-1 rounded-full text-xs font-semibold flex-shrink-0"
        :class="statusBadgeClass(item.primary_status)"
      >
        {{ statusLabel(item.primary_status) }}
      </span>
    </div>

    <!-- Metrics -->
    <MonitorMetricPair
      primary-icon="bolt"
      :primary-label="t('monitorCommon.dialogLatency')"
      :primary-value="formatLatency(item.primary_latency_ms)"
      primary-unit="ms"
      secondary-icon="globe"
      :secondary-label="t('monitorCommon.endpointPing')"
      :secondary-value="formatLatency(item.primary_ping_latency_ms)"
      secondary-unit="ms"
    />

    <!-- 配额模式：最新用量/余额快照（服务端已按系统开关剥离，此处 flag 为纵深防御） -->
    <MonitorQuotaView v-if="quotaVisible" :snapshot="item.latest_quota" class="mt-2" />

    <!-- Divider -->
    <div class="mt-4 border-t border-gray-100 dark:border-dark-700/60"></div>

    <!-- Availability row -->
    <MonitorAvailabilityRow
      :window-label="availabilityLabel"
      :value="availabilityValue"
      :samples-label="extraModelsCountLabel"
    />

    <!-- Timeline -->
    <MonitorTimeline
      :buckets="item.timeline"
      :countdown-seconds="countdownSeconds"
    />
  </button>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UserMonitorView } from '@/api/channelMonitor'
import {
  useChannelMonitorFormat,
  providerGradient,
} from '@/composables/useChannelMonitorFormat'
import { isChannelMonitorQuotaVisible } from '@/utils/featureFlags'
import { useAppStore } from '@/stores/app'
import { formatPeakRateWindow, hasPeakRate as groupHasPeakRate, serverTimezoneLabel } from '@/utils/peak-rate'
import ProviderIcon from './ProviderIcon.vue'
import MonitorMetricPair from './MonitorMetricPair.vue'
import MonitorAvailabilityRow from './MonitorAvailabilityRow.vue'
import MonitorTimeline from './MonitorTimeline.vue'
import MonitorQuotaView from '@/components/common/MonitorQuotaView.vue'

// 图标配色与 utils/platformColors.ts 的平台色对齐（新 4 家）。
const PROVIDER_TINT: Record<string, string> = {
  openai: 'text-emerald-600 dark:text-emerald-300',
  anthropic: 'text-orange-600 dark:text-orange-300',
  grok: 'text-zinc-700 dark:text-zinc-200',
  kimi: 'text-pink-600 dark:text-pink-300',
  zhipu: 'text-indigo-600 dark:text-indigo-300',
  deepseek: 'text-teal-600 dark:text-teal-300',
  opencode_go: 'text-amber-700 dark:text-amber-300',
}

const props = defineProps<{
  item: UserMonitorView
  window: '7d' | '15d' | '30d'
  availabilityValue: number | null
  countdownSeconds: number
  userGroupRates?: Record<number, number>
}>()

const emit = defineEmits<{
  (e: 'click'): void
}>()

const { t } = useI18n()
const appStore = useAppStore()
const {
  statusLabel,
  statusBadgeClass,
  formatLatency,
} = useChannelMonitorFormat()

const providerTintClass = computed(() =>
  PROVIDER_TINT[props.item.provider] ?? 'text-gray-500 dark:text-gray-300'
)

const quotaVisible = computed(
  () => isChannelMonitorQuotaVisible() && !!props.item.latest_quota
)

const userRateMultiplier = computed(() => {
  const groupId = props.item.group_id
  if (!groupId || !props.userGroupRates) return null
  return props.userGroupRates[groupId] ?? null
})

const hasCustomRate = computed(() => {
  const custom = userRateMultiplier.value
  const base = props.item.rate_multiplier
  return custom != null && base != null && custom !== base
})

const effectiveRate = computed(() => {
  if (hasCustomRate.value) return userRateMultiplier.value
  return props.item.rate_multiplier ?? null
})

const hasRate = computed(() => {
  const rate = effectiveRate.value
  return rate != null && !Number.isNaN(Number(rate))
})

const formattedDefaultRate = computed(() => formatRateValue(props.item.rate_multiplier))
const formattedEffectiveRate = computed(() => formatRateValue(effectiveRate.value))

function formatRateValue(value: number | null | undefined): string {
  if (value == null || Number.isNaN(Number(value))) return ''
  return `${Number(Number(value).toFixed(6))}x`
}

const hasPeakRate = computed(() => groupHasPeakRate(props.item))

const peakRateText = computed(() => {
  return formatPeakRateWindow(
    props.item,
    serverTimezoneLabel(appStore.cachedPublicSettings?.server_utc_offset),
  )
})

const peakRateTitle = computed(() => {
  return t('common.peakRateTooltip', { window: peakRateText.value })
})

const availabilityLabel = computed(() => {
  const win = t(`channelStatus.windowTab.${props.window}`)
  return `${t('monitorCommon.availabilityPrefix')} · ${win}`
})

const extraModelsCountLabel = computed(() => {
  const count = props.item.extra_models?.length ?? 0
  if (count === 0) return undefined
  return t('monitorCommon.extraModelsCount', { n: count })
})
</script>
