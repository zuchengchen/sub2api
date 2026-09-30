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
    // Vitest aliases the runtime-only bundle without production's JIT flag.
    messageCompiler: message => {
      if (typeof message !== 'string') throw new Error('Expected a source locale message')
      return new Function(`return ${baseCompile(message).code}`)()
    }
  })
  return mount(CookieWSStatus, { props: { ticket: value, compact }, global: { plugins: [i18n] } })
}

describe('CookieWSStatus', () => {
  it('shows zero actual sockets as recovering even when persisted cookies and the old ready flag say ready', () => {
    const wrapper = render(ticket({ cookie_groups_ready: 0, recovery_state: 'ready' }))
    expect(wrapper.get('[data-testid="cookie-ws-summary"]').text()).toBe('WS 0/3 · 恢复中')
    expect(wrapper.get('[data-testid="cookie-ws-summary"]').attributes('data-state')).toBe('recovering')
    expect(wrapper.get('[data-testid="cookie-ws-counts"]').text()).toContain('当前进程已验证 Cookie 组 0/3')
    expect(wrapper.get('[data-testid="cookie-ws-counts"]').text()).toContain('有效期内 Cookie 组 3/3')
  })

  it.each([1, 2])('shows %i actual sockets as partially available, then reacts to all three being ready', async count => {
    const wrapper = render(ticket({ verified_ws: count, recovery_state: 'partial' }))
    expect(wrapper.get('[data-testid="cookie-ws-summary"]').text()).toBe(`WS ${count}/3 · 部分可用`)
    await wrapper.setProps({ ticket: ticket({ verified_ws: 3, recovery_state: 'ready' }) })
    expect(wrapper.get('[data-testid="cookie-ws-summary"]').text()).toBe('WS 3/3 · 就绪')
  })

  it('keeps an unschedulable account paused despite existing sockets and displays the eligibility reason', () => {
    const wrapper = render(ticket({ verified_ws: 3, recovery_state: 'paused', skip_reason: 'rate_limited' }))
    expect(wrapper.get('[data-testid="cookie-ws-summary"]').text()).toBe('WS 3/3 · 已暂停')
    expect(wrapper.get('[data-testid="cookie-ws-pause-reason"]').text()).toBe('暂停原因：账号限流中')
  })

  it('distinguishes unavailable runtime data from a known zero socket count', () => {
    for (const value of [ticket({ verified_ws: undefined }), ticket({ recovery_state: 'unavailable' })]) {
      const wrapper = render(value)
      expect(wrapper.get('[data-testid="cookie-ws-summary"]').text()).toBe('WS —/3 · 状态不可读取')
      expect(wrapper.get('[data-testid="cookie-ws-summary"]').attributes('data-state')).toBe('unavailable')
      wrapper.unmount()
    }
  })

  it('does not describe a ready group with no diagnostic history as waiting for recovery', () => {
    const wrapper = render(ticket({ verified_ws: 3, recovery_state: 'ready', cookie_slots: [
      { slot: 0, state: 'ready', cookie_ready: true, verified_ws: 1 },
      { slot: 1, state: 'ready', cookie_ready: true, verified_ws: 1, refresh: { phase: 'idle', attempts: 1 } },
      { slot: 2, state: 'ready', cookie_ready: true, verified_ws: 1 }
    ] }))
    const first = wrapper.get('[data-testid="cookie-ws-slot-0"]')
    expect(first.text()).toContain('Cookie 刷新 · 暂无记录')
    expect(first.text()).toContain('WS 恢复 · 暂无记录')
    expect(first.text()).not.toContain('等待检查')
    expect(wrapper.get('[data-testid="cookie-ws-slot-1"]').text()).toContain('Cookie 刷新 · 无进行中任务')
  })

  it('shows independent refresh and warmup failures, phase, retry limits, and safe text for each group', () => {
    const retry = '2026-09-24T15:10:00Z'
    const failure = '2026-09-24T15:00:00Z'
    const value = ticket({
      recovery_state: 'recovering', cookie_groups_ready: 1,
      cookie_slots: [
        { slot: 0, state: 'backoff', cookie_ready: true, verified_ws: 0,
          refresh: { phase: 'ready', attempts: 1, last_success_at: '2026-09-24T14:50:00Z' },
          warmup: { phase: 'backoff', attempts: 3, last_attempt_at: failure, last_failure_at: failure, next_attempt_at: retry,
            last_error: { code: 'ws_validation_failed', message: '验证未通过 <img src=x>', http_status: 429, stage: 'ws_probe' } } },
        { slot: 1, state: 'harvesting', cookie_ready: false, verified_ws: 0,
          refresh: { phase: 'harvesting', attempts: 2 } },
        { slot: 2, state: 'restoring', cookie_ready: false, verified_ws: 0,
          refresh: { phase: 'restoring', attempts: 1 } }
      ]
    })
    const wrapper = render(value)
    const first = wrapper.get('[data-testid="cookie-ws-slot-0"]')
    expect(first.get('[data-testid="cookie-ws-refresh"]').text()).toContain('Cookie 刷新 · 就绪')
    expect(first.get('[data-testid="cookie-ws-refresh"]').text()).not.toContain('ws_validation_failed')
    const warmup = first.get('[data-testid="cookie-ws-warmup"]')
    expect(warmup.text()).toContain('WS 恢复 · 等待重试')
    expect(warmup.text()).toContain('尝试次数：3')
    expect(warmup.text()).toContain(`最早可重试：${formatDateTime(retry)}`)
    expect(warmup.text()).toContain(`上次失败：${formatDateTime(failure)}`)
    expect(warmup.get('[data-testid="cookie-ws-error"]').text()).toBe('验证未通过 <img src=x> (ws_validation_failed · ws_probe · HTTP 429)')
    expect(warmup.find('img').exists()).toBe(false)
    expect(wrapper.get('[data-testid="cookie-ws-slot-1"]').text()).toContain('获取 Cookie')
    expect(wrapper.get('[data-testid="cookie-ws-slot-2"]').text()).toContain('恢复已有 Cookie')
    expect(wrapper.text()).toContain('最早允许重试的时间')
    expect(wrapper.text()).toContain('后台检查轮次')

    const compact = render(value, true)
    expect(compact.find('[data-testid="cookie-ws-slot-0"]').exists()).toBe(false)
    const title = compact.get('[data-testid="cookie-ws-summary"]').attributes('title')
    expect(title).toContain('ws_validation_failed · ws_probe · HTTP 429')
    expect(title).toContain(`最早可重试：${formatDateTime(retry)}`)
    expect(title).toContain('第 3 组')
  })

  it('provides English recovery labels and scheduling explanations', () => {
    const wrapper = render(ticket({ verified_ws: 1, recovery_state: 'partial' }), false, 'en')
    expect(wrapper.get('[data-testid="cookie-ws-summary"]').text()).toBe('WS 1/3 · Partially available')
    expect(wrapper.text()).toContain('Retry times are the earliest allowed retry.')
  })

  describe('Tibo route chips', () => {
    const checked = '2026-09-30T07:40:00Z'
    const flipped = '2026-09-30T06:10:00Z'
    const nextProbe = '2026-09-30T07:50:00Z'
    const routes = (): NonNullable<CodexTurnTicketStatus['tibo_routes']> => [
      { route: 'http', verdict: 'healthy', confirmed: 'healthy', checked_at: checked, flipped_at: flipped, next_probe_at: nextProbe,
        last_sample: 'healthy', last_http: 200, last_answer: 'True', probes_hour: 6, flips_hour: 1 },
      { route: 'bps', verdict: 'degraded', confirmed: 'degraded', checked_at: checked, pending_votes: ['healthy', 'degraded'],
        last_sample: 'healthy', last_http: 429, probes_hour: 3, flips_hour: 0 },
      { route: 'cookie_ws', verdict: 'unavailable', probes_hour: 0, flips_hour: 0 }
    ]

    it('renders nothing new when tibo_routes is absent or empty', () => {
      for (const value of [ticket({ verified_ws: 3 }), ticket({ verified_ws: 3, tibo_routes: [] })]) {
        for (const compact of [false, true]) {
          const wrapper = render(value, compact)
          expect(wrapper.find('[data-testid="cookie-ws-routes"]').exists()).toBe(false)
          expect(wrapper.find('[data-testid="cookie-ws-route"]').exists()).toBe(false)
          wrapper.unmount()
        }
      }
    })

    it('renders one chip per route in backend order, colored by the effective verdict', () => {
      const wrapper = render(ticket({ verified_ws: 3, recovery_state: 'ready', tibo_routes: routes() }))
      const group = wrapper.get('[data-testid="cookie-ws-routes"]')
      expect(group.attributes('role')).toBe('group')
      expect(group.attributes('aria-label')).toBe('线路')
      const chips = wrapper.findAll('[data-testid="cookie-ws-route"]')
      expect(chips.map(chip => chip.attributes('data-route'))).toEqual(['http', 'bps', 'cookie_ws'])
      expect(chips.map(chip => chip.attributes('data-verdict'))).toEqual(['healthy', 'degraded', 'unavailable'])
      expect(chips.map(chip => chip.text())).toEqual(['HTTP · 健康', 'BPS · 降智', 'Cookie WS · 不可用'])
      expect(chips[0].classes()).toEqual(expect.arrayContaining(['bg-emerald-50', 'text-emerald-700']))
      expect(chips[1].classes()).toEqual(expect.arrayContaining(['bg-red-50', 'text-red-700']))
      expect(chips[2].classes()).toEqual(expect.arrayContaining(['bg-transparent', 'text-gray-400']))
      expect(chips[2].classes()).not.toContain('bg-gray-100')
    })

    it('uses the gray tone for unknown and explains a stale confirmation in the tooltip', () => {
      const wrapper = render(ticket({ verified_ws: 3, tibo_routes: [
        { route: 'http', verdict: 'unknown', confirmed: 'healthy', checked_at: checked, probes_hour: 0, flips_hour: 0 }
      ] }))
      const chip = wrapper.get('[data-testid="cookie-ws-route"]')
      expect(chip.text()).toBe('HTTP · 未知')
      expect(chip.classes()).toEqual(expect.arrayContaining(['bg-gray-100', 'text-gray-600']))
      expect(chip.attributes('title')!.split('\n')).toEqual([
        '线路 HTTP：未知',
        '已确认：健康',
        `最近检查：${formatDateTime(checked)}`,
        '近一小时探针/切换：0/0'
      ])
    })

    it('puts confirmed verdict, probe times, votes, last sample, and hourly counts in each chip tooltip', () => {
      const wrapper = render(ticket({ verified_ws: 3, tibo_routes: routes() }))
      const [http, bps, cookie] = wrapper.findAll('[data-testid="cookie-ws-route"]').map(chip => chip.attributes('title')!.split('\n'))
      expect(http).toEqual([
        '线路 HTTP：健康',
        '已确认：健康',
        `最近检查：${formatDateTime(checked)}`,
        `最近切换：${formatDateTime(flipped)}`,
        `下次探测：${formatDateTime(nextProbe)}`,
        '最近样本：健康 · HTTP 200 · True',
        '近一小时探针/切换：6/1'
      ])
      expect(bps).toContain('待确认投票：健康, 降智')
      expect(bps).toContain('最近样本：健康 · HTTP 429')
      expect(bps).toContain('近一小时探针/切换：3/0')
      expect(http.some(line => line.startsWith('投票窗口')), 'no calibration line without counters').toBe(false)
      // Cookie WS is never probed: only its readiness verdict is shown.
      expect(cookie).toEqual(['线路 Cookie WS：不可用'])
    })

    it('shows calibration counters in the tooltip when present', () => {
      const wrapper = render(ticket({ verified_ws: 3, tibo_routes: [
        { route: 'http', verdict: 'healthy', probes_hour: 4, flips_hour: 0, vote_windows: 5, vote_confirmed: 1, vote_rejected: 3 },
        { route: 'bps', verdict: 'healthy', probes_hour: 2, flips_hour: 0, shadow_agree: 7, shadow_disagree: 2 }
      ] }))
      const [http, bps] = wrapper.findAll('[data-testid="cookie-ws-route"]').map(chip => chip.attributes('title')!.split('\n'))
      expect(http).toContain('投票窗口 5：确认切换 1，否决 3（本次启动以来）')
      expect(bps).toContain('影子对比（与 HTTP 结论）：一致 7，不一致 2')
      expect(bps.some(line => line.startsWith('投票窗口'))).toBe(false)
    })

    it('keeps chips compact in the account list: label visible, verdict text for screen readers only', () => {
      const wrapper = render(ticket({ verified_ws: 3, tibo_routes: routes() }), true)
      expect(wrapper.find('[data-testid="cookie-ws-slot-0"]').exists()).toBe(false)
      const chips = wrapper.findAll('[data-testid="cookie-ws-route"]')
      expect(chips).toHaveLength(3)
      const verdicts = wrapper.findAll('[data-testid="cookie-ws-route-verdict"]')
      expect(verdicts.map(item => item.classes())).toEqual([['sr-only'], ['sr-only'], ['sr-only']])
      expect(verdicts.map(item => item.text())).toEqual(['· 健康', '· 降智', '· 不可用'])
      expect(chips[1].attributes('data-verdict')).toBe('degraded')
      expect(chips[1].attributes('title')).toContain('待确认投票：健康, 降智')
    })

    it('provides English route labels', () => {
      const wrapper = render(ticket({ verified_ws: 3, tibo_routes: routes() }), false, 'en')
      expect(wrapper.get('[data-testid="cookie-ws-routes"]').attributes('aria-label')).toBe('Routes')
      const chips = wrapper.findAll('[data-testid="cookie-ws-route"]')
      expect(chips.map(chip => chip.text())).toEqual(['HTTP · Healthy', 'BPS · Degraded', 'Cookie WS · Unavailable'])
      const bps = chips[1].attributes('title')!
      expect(bps).toContain('Route BPS: Degraded')
      expect(bps).toContain('Pending votes: Healthy, Degraded')
      expect(bps).toContain('Probes/flips in the last hour: 3/0')
    })
  })
})
