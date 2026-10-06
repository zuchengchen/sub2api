import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { baseCompile } from '@intlify/message-compiler'
import CookieWSStatus from '../CookieWSStatus.vue'
import type { CodexTurnTicketStatus } from '@/types'
import zh from '@/i18n/locales/zh/admin/accounts'
import en from '@/i18n/locales/en/admin/accounts'
import { formatDateTime } from '@/utils/format'

function ticket(overrides: Partial<CodexTurnTicketStatus> = {}): CodexTurnTicketStatus {
  return {
    model: 'gpt-6-astra', mode: 'cookie_ws', ready: true, blocked: false,
    remaining_seconds: 2500, cookie_groups_ready: 3, cookie_groups_valid: 3,
    cookie_groups_total: 3, ws_per_group: 1, verified_ws: 0, minimum_ws: 3,
    ...overrides
  }
}

function render(value = ticket(), compact = false, locale = 'zh') {
  const i18n = createI18n({
    legacy: false, locale, messages: { zh: { admin: zh }, en: { admin: en } },
    messageCompiler: message => {
      if (typeof message !== 'string') throw new Error('Expected a source locale message')
      return new Function(`return ${baseCompile(message).code}`)()
    }
  })
  return mount(CookieWSStatus, { props: { ticket: value, compact }, global: { plugins: [i18n] } })
}

describe('CookieWSStatus', () => {
  const checked = '2026-09-30T07:40:00Z'
  const flipped = '2026-09-30T06:10:00Z'
  const nextProbe = '2026-09-30T07:50:00Z'
  const routes = (): NonNullable<CodexTurnTicketStatus['tibo_routes']> => [
    { route: 'http', verdict: 'healthy', confirmed: 'healthy', checked_at: checked, flipped_at: flipped, next_probe_at: nextProbe,
      last_sample: 'healthy', last_http: 200, last_answer: 'True', probes_hour: 6, flips_hour: 1 },
    { route: 'bps', verdict: 'degraded', confirmed: 'degraded', checked_at: checked, pending_votes: ['healthy', 'degraded'],
      last_sample: 'healthy', last_http: 429, probes_hour: 3, flips_hour: 0 },
    { route: 'ticket', verdict: 'unknown', probes_hour: 0, flips_hour: 0 }
  ]

  it('hides Cookie WS recovery and only shows the HTTP Tibo chip', () => {
    const wrapper = render(ticket({ verified_ws: 1, recovery_state: 'partial', tibo_routes: routes() }))
    expect(wrapper.find('[data-testid="cookie-ws-summary"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="cookie-ws-slot-0"]').exists()).toBe(false)
    const chips = wrapper.findAll('[data-testid="cookie-ws-route"]')
    expect(chips.map(chip => chip.attributes('data-route'))).toEqual(['http'])
    expect(chips.map(chip => chip.text())).toEqual(['HTTP · 健康'])
  })

  it('renders nothing when tibo_routes has no HTTP route', () => {
    for (const value of [ticket({ verified_ws: 3 }), ticket({ verified_ws: 3, tibo_routes: [{ route: 'bps', verdict: 'unknown' }] })]) {
      const wrapper = render(value)
      expect(wrapper.find('[data-testid="cookie-ws-status"]').exists()).toBe(false)
      wrapper.unmount()
    }
  })

  it('puts HTTP probe details in the chip tooltip', () => {
    const wrapper = render(ticket({ tibo_routes: routes() }))
    expect(wrapper.get('[data-testid="cookie-ws-route"]').attributes('title')!.split('\n')).toEqual([
      '线路 HTTP：健康',
      '已确认：健康',
      `最近检查：${formatDateTime(checked)}`,
      `最近切换：${formatDateTime(flipped)}`,
      `下次探测：${formatDateTime(nextProbe)}`,
      '最近样本：健康 · HTTP 200 · True',
      '近一小时探针/切换：6/1'
    ])
  })

  it('keeps the HTTP verdict for screen readers in the compact account list', () => {
    const wrapper = render(ticket({ tibo_routes: routes() }), true)
    expect(wrapper.find('[data-testid="cookie-ws-summary"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="cookie-ws-route"]')).toHaveLength(1)
    expect(wrapper.get('[data-testid="cookie-ws-route-verdict"]').classes()).toContain('sr-only')
    expect(wrapper.get('[data-testid="cookie-ws-route-verdict"]').text()).toBe('· 健康')
  })

  it('provides English HTTP labels', () => {
    const wrapper = render(ticket({ tibo_routes: routes() }), false, 'en')
    expect(wrapper.get('[data-testid="cookie-ws-routes"]').attributes('aria-label')).toBe('Routes')
    expect(wrapper.get('[data-testid="cookie-ws-route"]').text()).toBe('HTTP · Healthy')
  })
})
