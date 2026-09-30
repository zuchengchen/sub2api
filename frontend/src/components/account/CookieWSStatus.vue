<template>
  <div :class="compact ? 'inline-flex' : 'space-y-2'" data-testid="cookie-ws-status">
    <span
      :class="statusClass"
      :title="diagnosticTitle"
      :data-state="state"
      data-testid="cookie-ws-summary"
    >{{ summary }}</span>
    <span
      v-if="tiboRoutes.length"
      role="group"
      :aria-label="t('admin.accounts.openai.codexTiboRoutes')"
      :class="compact ? 'ml-1 inline-flex items-center gap-1' : 'flex flex-wrap items-center gap-1'"
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
    <template v-if="!compact">
      <p class="text-xs text-gray-500 dark:text-gray-400" data-testid="cookie-ws-counts">{{ groupCounts }}</p>
      <p v-if="skipReason" class="text-xs text-amber-600 dark:text-amber-400" data-testid="cookie-ws-pause-reason">
        {{ skipReason }}
      </p>
      <div class="grid gap-2 sm:grid-cols-3">
        <div
          v-for="slot in slots"
          :key="slot.slot"
          class="min-w-0 rounded border border-gray-200 p-2 text-xs dark:border-dark-600"
          :data-testid="`cookie-ws-slot-${slot.slot}`"
        >
          <div class="flex flex-wrap items-center justify-between gap-1 font-medium">
            <span>{{ t('admin.accounts.openai.codexCookieSlot', { number: slot.slot + 1 }) }}</span>
            <span>{{ phaseLabel(slot.state) }}</span>
          </div>
          <p class="mt-1 text-gray-500 dark:text-gray-400">
            {{ t(slot.cookie_ready ? 'admin.accounts.openai.codexCookieSlotVerified' : 'admin.accounts.openai.codexCookieSlotUnverified') }}
            · {{ t('admin.accounts.openai.codexCookieSlotSockets', { count: slot.verified_ws, total: ticket.ws_per_group ?? 1 }) }}
          </p>
          <p v-if="slot.captured_at" class="mt-1 text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.openai.codexCookieCapturedAt', { time: displayTime(slot.captured_at) }) }}
          </p>
          <p v-if="slot.refresh_at" class="text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.openai.codexCookieRefreshAt', { time: displayTime(slot.refresh_at) }) }}
          </p>
          <p v-if="slot.expires_at" class="text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.openai.codexCookieExpiresAt', { time: displayTime(slot.expires_at) }) }}
          </p>
          <div v-for="kind in diagnosticKinds" :key="kind" class="mt-2 border-t border-gray-100 pt-2 dark:border-dark-700" :data-testid="`cookie-ws-${kind}`">
            <p class="font-medium">
              {{ diagnosticLabel(kind) }} · {{ phaseLabel(slot[kind]?.phase ?? 'no_record') }}
            </p>
            <template v-if="slot[kind]">
              <p class="mt-1 text-gray-500 dark:text-gray-400">
                {{ t('admin.accounts.openai.codexCookieAttempts', { count: slot[kind]!.attempts }) }}
              </p>
              <p v-if="slot[kind]!.last_attempt_at" class="text-gray-500 dark:text-gray-400">
                {{ t('admin.accounts.openai.codexCookieLastAttempt', { time: displayTime(slot[kind]!.last_attempt_at!) }) }}
              </p>
              <p v-if="slot[kind]!.last_success_at" class="text-gray-500 dark:text-gray-400">
                {{ t('admin.accounts.openai.codexCookieLastSuccess', { time: displayTime(slot[kind]!.last_success_at!) }) }}
              </p>
              <p v-if="slot[kind]!.last_failure_at" class="text-gray-500 dark:text-gray-400">
                {{ t('admin.accounts.openai.codexCookieLastFailure', { time: displayTime(slot[kind]!.last_failure_at!) }) }}
              </p>
              <p v-if="slot[kind]!.next_attempt_at" class="mt-1 text-gray-500 dark:text-gray-400">
                {{ t('admin.accounts.openai.codexCookieNextAttempt', { time: displayTime(slot[kind]!.next_attempt_at!) }) }}
              </p>
              <p v-if="slot[kind]!.last_error" class="mt-1 break-words text-amber-600 dark:text-amber-400" data-testid="cookie-ws-error">
                {{ errorText(slot[kind]!.last_error!) }}
              </p>
            </template>
          </div>
        </div>
      </div>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.codexCookieRetryHint') }}</p>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { CodexTurnTicketStatus, OpenAICookieWSRecoveryDiagnostic, OpenAICookieWSRecoveryState, OpenAICookieWSSlotStatus, TiboRouteStatus } from '@/types'
import { formatDateTime } from '@/utils/format'

const props = withDefaults(defineProps<{ ticket: CodexTurnTicketStatus; compact?: boolean }>(), { compact: false })
const { t } = useI18n()
const diagnosticKinds = ['refresh', 'warmup'] as const
const phases = new Set(['idle', 'no_record', 'ready', 'waiting', 'restoring', 'harvesting', 'validating', 'warming', 'backoff', 'paused', 'unavailable'])
const skipReasons = new Set(['not_cookie_account', 'not_active', 'manual_unschedulable', 'expired', 'rate_limited', 'overloaded', 'temporarily_unschedulable', 'model_rate_limited', 'quota_5h', 'quota_7d', 'runtime_blocked', 'model_temporarily_unschedulable'])

const verifiedWS = computed(() => Math.max(0, props.ticket.verified_ws ?? 0))
const targetWS = computed(() => props.ticket.minimum_ws ?? 3)
const state = computed<OpenAICookieWSRecoveryState>(() => {
  if (props.ticket.recovery_state === 'paused' || props.ticket.skip_reason) return 'paused'
  if (props.ticket.recovery_state === 'unavailable' || props.ticket.verified_ws === undefined) return 'unavailable'
  if (verifiedWS.value >= targetWS.value) return 'ready'
  return verifiedWS.value > 0 ? 'partial' : 'recovering'
})
const summary = computed(() => t(`admin.accounts.openai.codexCookieRecovery.${state.value}`, { count: verifiedWS.value, total: targetWS.value }))
const statusClass = computed(() => {
  if (state.value === 'ready') return 'text-emerald-600 dark:text-emerald-400'
  if (state.value === 'partial' || state.value === 'paused') return 'text-amber-600 dark:text-amber-400'
  if (state.value === 'recovering') return 'text-sky-600 dark:text-sky-400'
  return 'text-gray-500 dark:text-gray-400'
})
const groupCounts = computed(() => t('admin.accounts.openai.codexCookieGroupCounts', {
  ready: props.ticket.cookie_groups_ready ?? 0,
  valid: props.ticket.cookie_groups_valid ?? '—',
  total: props.ticket.cookie_groups_total ?? 3
}))
const skipReason = computed(() => {
  const reason = props.ticket.skip_reason
  if (!reason) return ''
  const label = skipReasons.has(reason) ? t(`admin.accounts.openai.codexCookieSkipReason.${reason}`) : reason
  return t('admin.accounts.openai.codexCookiePauseReason', { reason: label })
})
const slots = computed<OpenAICookieWSSlotStatus[]>(() => Array.from({ length: props.ticket.cookie_groups_total ?? 3 }, (_, slot) =>
  props.ticket.cookie_slots?.find(item => item.slot === slot) ?? { slot, state: 'unavailable', cookie_ready: false, verified_ws: 0 }
))

function displayTime(value: string): string {
  return formatDateTime(value)
}

function phaseLabel(phase: string): string {
  return phases.has(phase) ? t(`admin.accounts.openai.codexCookiePhase.${phase}`) : phase
}

function diagnosticLabel(kind: typeof diagnosticKinds[number]): string {
  return t(kind === 'refresh' ? 'admin.accounts.openai.codexCookieRefreshDiagnostic' : 'admin.accounts.openai.codexCookieWarmupDiagnostic')
}

function errorText(error: NonNullable<OpenAICookieWSRecoveryDiagnostic['last_error']>): string {
  const details = [error.code, error.stage, error.http_status ? `HTTP ${error.http_status}` : ''].filter(Boolean).join(' · ')
  return details ? `${error.message} (${details})` : error.message
}

// Tibo route chips: one per route, colored by the effective verdict used for routing.
const tiboRoutes = computed<TiboRouteStatus[]>(() => props.ticket.tibo_routes ?? [])
const routeLabels: Record<string, string> = { http: 'HTTP', bps: 'BPS', cookie_ws: 'Cookie WS' }
const routeVerdicts = new Set(['healthy', 'unknown', 'degraded', 'unavailable'])
const routeVerdictClasses: Record<string, string> = {
  healthy: 'bg-emerald-50 text-emerald-700 ring-emerald-200 dark:bg-emerald-500/10 dark:text-emerald-300 dark:ring-emerald-500/30',
  unknown: 'bg-gray-100 text-gray-600 ring-gray-200 dark:bg-dark-700 dark:text-gray-300 dark:ring-dark-500',
  degraded: 'bg-red-50 text-red-700 ring-red-200 dark:bg-red-500/10 dark:text-red-300 dark:ring-red-500/30',
  unavailable: 'bg-transparent text-gray-400 ring-gray-200 dark:text-gray-500 dark:ring-dark-600'
}

function routeLabel(route: TiboRouteStatus): string {
  return routeLabels[route.route] ?? route.route
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
  if (route.checked_at) lines.push(t('admin.accounts.openai.codexTiboRouteCheckedAt', { time: displayTime(route.checked_at) }))
  if (route.flipped_at) lines.push(t('admin.accounts.openai.codexTiboRouteFlippedAt', { time: displayTime(route.flipped_at) }))
  if (route.next_probe_at) lines.push(t('admin.accounts.openai.codexTiboRouteNextProbeAt', { time: displayTime(route.next_probe_at) }))
  if (route.pending_votes?.length) {
    lines.push(t('admin.accounts.openai.codexTiboRoutePendingVotes', { votes: route.pending_votes.map(routeVerdictLabel).join(', ') }))
  }
  if (route.last_sample) {
    const sample = [routeVerdictLabel(route.last_sample), route.last_http ? `HTTP ${route.last_http}` : '', route.last_answer ?? ''].filter(Boolean).join(' · ')
    lines.push(t('admin.accounts.openai.codexTiboRouteLastSample', { sample }))
  }
  // Cookie WS is never probed, so its hourly counters are always zero; omit them.
  if (route.route !== 'cookie_ws') {
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
  }
  return lines.join('\n')
}

const diagnosticTitle = computed(() => {
  const lines = [summary.value, groupCounts.value]
  if (skipReason.value) lines.push(skipReason.value)
  for (const slot of slots.value) {
    lines.push(`${t('admin.accounts.openai.codexCookieSlot', { number: slot.slot + 1 })}: ${phaseLabel(slot.state)} · WS ${slot.verified_ws}/${props.ticket.ws_per_group ?? 1}`)
    for (const kind of diagnosticKinds) {
      const diagnostic = slot[kind]
      if (!diagnostic) continue
      const label = diagnosticLabel(kind)
      lines.push(`${label}: ${phaseLabel(diagnostic.phase)}`)
      if (diagnostic.last_error) lines.push(`${label}: ${errorText(diagnostic.last_error)}`)
      if (diagnostic.next_attempt_at) lines.push(`${label}: ${t('admin.accounts.openai.codexCookieNextAttempt', { time: displayTime(diagnostic.next_attempt_at) })}`)
    }
  }
  lines.push(t('admin.accounts.openai.codexCookieRetryHint'))
  return lines.join('\n')
})
</script>
