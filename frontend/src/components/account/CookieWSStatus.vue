<template>
  <div v-if="tiboRoutes.length" :class="compact ? 'inline-flex' : 'space-y-2'" data-testid="cookie-ws-status">
    <span
      role="group"
      :aria-label="t('admin.accounts.openai.codexTiboRoutes')"
      :class="compact ? 'inline-flex items-center gap-1' : 'flex flex-wrap items-center gap-1'"
      data-testid="cookie-ws-routes"
    >
      <span
        v-for="route in tiboRoutes"
        :key="route.route"
        :class="routeChipClass(route)"
        :title="routeTitle(route)"
        :data-route="route.route"
        :data-verdict="route.verdict"
        data-testid="cookie-ws-route"
      >{{ routeLabel(route) }}<span :class="compact ? 'sr-only' : ''" data-testid="cookie-ws-route-verdict">{{ ' · ' + routeVerdictLabel(route.verdict) }}</span></span>
    </span>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { CodexTurnTicketStatus, TiboRouteStatus } from '@/types'
import { formatDateTime } from '@/utils/format'

const props = withDefaults(defineProps<{ ticket: CodexTurnTicketStatus; compact?: boolean }>(), { compact: false })
const { t } = useI18n()

const tiboRoutes = computed<TiboRouteStatus[]>(() =>
  (props.ticket.tibo_routes ?? []).filter(route => route.route === 'http')
)
const routeVerdicts = new Set(['healthy', 'unknown', 'degraded', 'unavailable'])
const routeVerdictClasses: Record<string, string> = {
  healthy: 'bg-emerald-50 text-emerald-700 ring-emerald-200 dark:bg-emerald-500/10 dark:text-emerald-300 dark:ring-emerald-500/30',
  unknown: 'bg-gray-100 text-gray-600 ring-gray-200 dark:bg-dark-700 dark:text-gray-300 dark:ring-dark-500',
  degraded: 'bg-red-50 text-red-700 ring-red-200 dark:bg-red-500/10 dark:text-red-300 dark:ring-red-500/30',
  unavailable: 'bg-transparent text-gray-400 ring-gray-200 dark:text-gray-500 dark:ring-dark-600'
}

function routeLabel(_route: TiboRouteStatus): string {
  return 'HTTP'
}

function routeVerdictLabel(verdict: string): string {
  return routeVerdicts.has(verdict) ? t(`admin.accounts.openai.codexTiboRouteVerdict.${verdict}`) : verdict
}

function routeChipClass(route: TiboRouteStatus): string {
  const tone = routeVerdictClasses[route.verdict] ?? routeVerdictClasses.unknown
  return `inline-flex items-center whitespace-nowrap rounded px-1 py-px text-[10px] font-medium leading-4 ring-1 ring-inset ${tone}`
}

function routeTitle(route: TiboRouteStatus): string {
  const lines = [t('admin.accounts.openai.codexTiboRouteSummary', { route: routeLabel(route), verdict: routeVerdictLabel(route.verdict) })]
  if (route.confirmed) lines.push(t('admin.accounts.openai.codexTiboRouteConfirmed', { verdict: routeVerdictLabel(route.confirmed) }))
  if (route.checked_at) lines.push(t('admin.accounts.openai.codexTiboRouteCheckedAt', { time: formatDateTime(route.checked_at) }))
  if (route.flipped_at) lines.push(t('admin.accounts.openai.codexTiboRouteFlippedAt', { time: formatDateTime(route.flipped_at) }))
  if (route.next_probe_at) lines.push(t('admin.accounts.openai.codexTiboRouteNextProbeAt', { time: formatDateTime(route.next_probe_at) }))
  if (route.pending_votes?.length) {
    lines.push(t('admin.accounts.openai.codexTiboRoutePendingVotes', { votes: route.pending_votes.map(routeVerdictLabel).join(', ') }))
  }
  if (route.last_sample) {
    const sample = [routeVerdictLabel(route.last_sample), route.last_http ? `HTTP ${route.last_http}` : '', route.last_answer ?? ''].filter(Boolean).join(' · ')
    lines.push(t('admin.accounts.openai.codexTiboRouteLastSample', { sample }))
  }
  lines.push(t('admin.accounts.openai.codexTiboRouteHourly', { probes: route.probes_hour ?? 0, flips: route.flips_hour ?? 0 }))
  if (route.vote_windows) {
    lines.push(t('admin.accounts.openai.codexTiboRouteVotes', {
      windows: route.vote_windows,
      confirmed: route.vote_confirmed ?? 0,
      rejected: route.vote_rejected ?? 0
    }))
  }
  if (route.shadow_agree || route.shadow_disagree) {
    lines.push(t('admin.accounts.openai.codexTiboRouteShadow', {
      agree: route.shadow_agree ?? 0,
      disagree: route.shadow_disagree ?? 0
    }))
  }
  return lines.join('\n')
}
</script>
