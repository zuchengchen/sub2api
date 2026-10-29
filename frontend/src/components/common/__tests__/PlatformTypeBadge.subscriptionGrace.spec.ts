import { enableAutoUnmount, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'

import en from '@/i18n/locales/en/admin/accounts'
import zh from '@/i18n/locales/zh/admin/accounts'
import type { AccountPlatform } from '@/types'
import { formatDateLocalInput } from '@/utils/format'
import PlatformTypeBadge from '../PlatformTypeBadge.vue'

// Compile the real locale messages in tests, as the production build does.
vi.mock('vue-i18n', async () => {
  const { createRequire } = await import('node:module')
  return vi.importActual<typeof import('vue-i18n')>(
    createRequire(import.meta.url).resolve('vue-i18n/dist/vue-i18n.esm-bundler.js')
  )
})

enableAutoUnmount(afterEach)
beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-10-20T12:00:00Z'))
})
afterEach(() => vi.useRealTimers())

const expiration = '2026-10-28T12:34:56Z'
const graceSelector = '[data-testid="subscription-grace"]'

function mountBadge(props: {
  platform?: AccountPlatform
  planType?: string
  subscriptionExpiresAt?: string
} = {}, locale: 'en' | 'zh' = 'en') {
  return mount(PlatformTypeBadge, {
    props: { platform: 'openai', type: 'oauth', planType: 'pro', subscriptionExpiresAt: expiration, ...props },
    global: {
      plugins: [createI18n({ legacy: false, locale, messages: { en: { admin: en }, zh: { admin: zh } } })],
      stubs: { PlatformIcon: true, Icon: true }
    }
  })
}

function localDateTime(date: Date, locale: 'en' | 'zh' = 'en') {
  return new Intl.DateTimeFormat(locale, {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false
  }).format(date)
}

describe('PlatformTypeBadge subscription grace', () => {
  it.each([
    ['pro', 14, '2026-11-11T12:34:56Z'],
    ['chatgptpro', 14, '2026-11-11T12:34:56Z'],
    [' PRO ', 14, '2026-11-11T12:34:56Z'],
    ['ChatGPT-Pro', 14, '2026-11-11T12:34:56Z'],
    ['prolite', 14, '2026-11-11T12:34:56Z'],
    ['Pro_Lite', 14, '2026-11-11T12:34:56Z'],
    ['promax', 14, '2026-11-11T12:34:56Z'],
    ['Pro Max', 14, '2026-11-11T12:34:56Z'],
    ['plus', 30, '2026-11-27T12:34:56Z'],
    [' PLUS ', 30, '2026-11-27T12:34:56Z']
  ])('shows %s with %i days separately from subscription expiry', (planType, days, until) => {
    const wrapper = mountBadge({ planType })
    const grace = wrapper.get(graceSelector)

    expect(wrapper.text()).toContain(`Expires ${formatDateLocalInput(new Date(expiration))}`)
    expect(grace.text()).toBe(`Grace until ${formatDateLocalInput(new Date(until))}`)
    expect(grace.attributes('title')).toContain(`${days} days (24 hours each)`)
    expect(grace.attributes('title')).toContain(`${localDateTime(new Date(until))} (local time)`)
    expect(grace.attributes('title')).toContain("Estimated using this site's rule")
    expect(grace.attributes('title')).toContain('actual availability depends on the upstream provider')
  })

  it.each([
    ['2026-10-28T12:34:55.999Z', '', 'text-gray-400'],
    [expiration, 'In grace period', 'text-amber-600'],
    ['2026-11-11T12:34:55.999Z', 'In grace period', 'text-amber-600'],
    ['2026-11-11T12:34:56Z', 'Grace period ended', 'text-red-500'],
    ['2026-11-12T12:34:56Z', 'Grace period ended', 'text-red-500']
  ])('renders the correct status at %s', (now, status, color) => {
    vi.setSystemTime(new Date(now))
    const grace = mountBadge().get(graceSelector)

    expect(grace.text()).toBe(`Grace until ${formatDateLocalInput(new Date('2026-11-11T12:34:56Z'))}${status ? ` ${status}` : ''}`)
    expect(grace.classes()).toContain(color)
  })

  it.each(['pro', 'plus'])('updates both boundaries while %s stays mounted', async (planType) => {
    vi.setSystemTime(new Date('2026-10-28T12:34:55.999Z'))
    const wrapper = mountBadge({ planType })
    expect(wrapper.get(graceSelector).text()).not.toContain('In grace period')

    await vi.advanceTimersByTimeAsync(1)
    expect(wrapper.get(graceSelector).text()).toContain('In grace period')

    const end = new Date(planType === 'plus' ? '2026-11-27T12:34:56Z' : '2026-11-11T12:34:56Z')
    await vi.advanceTimersByTimeAsync(end.getTime() - Date.now() - 1)
    expect(wrapper.get(graceSelector).text()).toContain('In grace period')
    await vi.advanceTimersByTimeAsync(1)
    expect(wrapper.get(graceSelector).text()).toContain('Grace period ended')
  })

  it('reschedules after a subscription renewal and clears timers on unmount', async () => {
    vi.setSystemTime(new Date(expiration))
    const wrapper = mountBadge()
    expect(wrapper.get(graceSelector).text()).toContain('In grace period')

    await wrapper.setProps({ subscriptionExpiresAt: '2026-12-28T12:34:56Z' })
    expect(wrapper.get(graceSelector).text()).not.toContain('In grace period')
    await vi.advanceTimersByTimeAsync(new Date('2026-11-11T12:34:56Z').getTime() - Date.now())
    expect(wrapper.get(graceSelector).text()).not.toContain('Grace period ended')

    wrapper.unmount()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('removes grace and its timer when the plan changes to an ineligible tier', async () => {
    const wrapper = mountBadge()
    expect(wrapper.find(graceSelector).exists()).toBe(true)

    await wrapper.setProps({ planType: 'team' })
    expect(wrapper.find(graceSelector).exists()).toBe(false)
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each([
    'free', 'basic', 'xbasic', 'business', 'team', 'unknown', 'go', 'enterprise',
    'self_serve_business_prolite', 'self_serve_business_usage_based', 'edu_pro', 'future_sku', '', undefined
  ])('does not estimate grace for %s', (planType) => {
    expect(mountBadge({ planType }).find(graceSelector).exists()).toBe(false)
  })

  it.each<AccountPlatform>(['anthropic', 'antigravity', 'grok', 'gemini'])('does not apply grace to %s', (platform) => {
    const wrapper = mountBadge({ platform })
    expect(wrapper.find(graceSelector).exists()).toBe(false)
    expect(wrapper.text()).toContain(`Expires ${formatDateLocalInput(new Date(expiration))}`)
  })

  it.each([
    undefined, '', ' ', 'invalid', '2026-13-01T12:00:00Z', '2026-10-28T25:00:00Z',
    '2026-02-29T12:00:00Z', '2026-02-30T12:00:00+08:00'
  ])('hides missing or invalid expiry %s', (subscriptionExpiresAt) => {
    const wrapper = mountBadge({ subscriptionExpiresAt })
    expect(wrapper.find(graceSelector).exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Expires')
    expect(wrapper.text()).not.toContain('Invalid Date')
  })

  it('hides grace when adding days exceeds the supported date range', () => {
    const wrapper = mountBadge({ subscriptionExpiresAt: '+275760-09-13T00:00:00.000Z' })
    expect(wrapper.find(graceSelector).exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Invalid Date')
  })

  // Run this suite under America/New_York as well as UTC to exercise DST shifts.
  it.each([
    ['pro', '2028-02-29T12:34:56Z', '2028-03-14T12:34:56Z'],
    ['pro', '2026-03-01T23:30:00-05:00', '2026-03-16T04:30:00Z'],
    ['plus', '2026-02-15T23:30:00-05:00', '2026-03-18T04:30:00Z'],
    ['pro', '2026-10-25T00:30:00-04:00', '2026-11-08T04:30:00Z'],
    ['plus', '2026-10-15T00:30:00-04:00', '2026-11-14T04:30:00Z']
  ])('adds fixed 24-hour days for %s across DST from %s', (planType, subscriptionExpiresAt, until) => {
    const grace = mountBadge({ planType, subscriptionExpiresAt }).get(graceSelector)
    expect(grace.text()).toContain(`Grace until ${formatDateLocalInput(new Date(until))}`)
    expect(grace.attributes('title')).toContain(localDateTime(new Date(until)))
  })

  it('renders the Chinese date, state, days and estimate notice', () => {
    vi.setSystemTime(new Date(expiration))
    const grace = mountBadge({ planType: 'plus' }, 'zh').get(graceSelector)
    const until = new Date('2026-11-27T12:34:56Z')
    expect(grace.text()).toBe(`宽限期至 ${formatDateLocalInput(until)} 宽限期中`)
    expect(grace.attributes('title')).toContain('30 天（每天 24 小时）')
    expect(grace.attributes('title')).toContain(`${localDateTime(until, 'zh')}（本地时间）`)
    expect(grace.attributes('title')).toContain('按本站规则估算')
    expect(grace.attributes('title')).toContain('实际可用性以上游为准')
  })
})
