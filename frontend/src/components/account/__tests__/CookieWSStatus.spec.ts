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
})
