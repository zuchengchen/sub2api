import { enableAutoUnmount, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
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
afterEach(() => vi.useRealTimers())

const expiration = '2026-10-28T12:34:56Z'
const expirySelector = '[data-testid="subscription-expiry"]'
const graceSelector = '[data-testid="subscription-grace"]'

function mountBadge(props: {
  platform?: AccountPlatform
  planType?: string
  subscriptionExpiresAt?: string
  subscriptionIsDelinquent?: boolean | null
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
    ['pro', '2026-11-11T12:34:56Z'],
    ['chatgptpro', '2026-11-11T12:34:56Z'],
    [' PRO ', '2026-11-11T12:34:56Z'],
    ['ChatGPT-Pro', '2026-11-11T12:34:56Z'],
    ['prolite', '2026-11-11T12:34:56Z'],
    ['Pro_Lite', '2026-11-11T12:34:56Z'],
    ['promax', '2026-11-11T12:34:56Z'],
    ['Pro Max', '2026-11-11T12:34:56Z'],
    ['plus', '2026-11-27T12:34:56Z'],
    [' PLUS ', '2026-11-27T12:34:56Z']
  ])('shows the %s grace date separately from subscription expiry', (planType, until) => {
    const wrapper = mountBadge({ planType })
    const grace = wrapper.get(graceSelector)

    expect(wrapper.get(expirySelector).text()).toBe(`Expires ${formatDateLocalInput(new Date(expiration))}`)
    expect(grace.text()).toBe(`Grace ${formatDateLocalInput(new Date(until))}`)
    expect(grace.attributes('title')).toBe(localDateTime(new Date(until)))
  })

  it.each([
    '2026-10-28T12:34:55.999Z', expiration, '2026-11-11T12:34:55.999Z',
    '2026-11-11T12:34:56Z', '2026-11-12T12:34:56Z'
  ])('renders only two plain Chinese date lines regardless of the current time %s', (now) => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(now))
    const wrapper = mountBadge({}, 'zh')
    const lines = wrapper.findAll('[data-testid^="subscription-"]')

    expect(lines.map(line => line.text())).toEqual(['到期 2026-10-28', '宽限 2026-11-11'])
    for (const line of lines) {
      expect(line.classes()).toContain('text-gray-400')
      expect(line.classes()).toContain('dark:text-gray-500')
      expect(line.classes().join(' ')).not.toMatch(/text-(amber|red)-/)
    }
    expect(wrapper.html()).not.toMatch(/宽限期中|已结束|预计|估算|In grace period|Grace period ended|Estimated/)
  })

  it.each([false, null, undefined])('keeps the original entitlement date when delinquency is %s', (subscriptionIsDelinquent) => {
    const wrapper = mountBadge({ subscriptionIsDelinquent })
    expect(wrapper.get(expirySelector).text()).toBe('Expires 2026-10-28')
    expect(wrapper.get(expirySelector).attributes('title')).toBe(localDateTime(new Date(expiration)))
    expect(wrapper.get(graceSelector).text()).toBe('Grace 2026-11-11')
  })

  it.each([
    ['2026-11-28T12:34:56', '2026-10-28T12:34:56'],
    ['2026-03-31T12:34:56', '2026-02-28T12:34:56'],
    ['2028-03-31T12:34:56', '2028-02-29T12:34:56'],
    ['2026-01-31T12:34:56', '2025-12-31T12:34:56'],
    ['2026-05-31T12:34:56', '2026-04-30T12:34:56']
  ])('moves delinquent expiry %s back one local calendar month to %s', (subscriptionExpiresAt, adjusted) => {
    const wrapper = mountBadge({ subscriptionExpiresAt, subscriptionIsDelinquent: true }, 'zh')
    const until = new Date(new Date(adjusted).getTime() + 14 * 24 * 60 * 60 * 1000)

    expect(wrapper.get(expirySelector).text()).toBe(`到期 ${adjusted.slice(0, 10)}`)
    expect(wrapper.get(expirySelector).attributes('title')).toBe(localDateTime(new Date(adjusted), 'zh'))
    expect(wrapper.get(graceSelector).text()).toBe(`宽限 ${formatDateLocalInput(until)}`)
    expect(wrapper.get(graceSelector).attributes('title')).toBe(localDateTime(until, 'zh'))
  })

  // Run in UTC, Asia/Shanghai and America/New_York to catch both UTC date boundaries.
  it.each([
    { local: '2026-03-01T00:30:45', adjusted: '2026-02-01T00:30:45', delinquent: true },
    { local: '2026-02-28T23:30:45', adjusted: '2026-01-28T23:30:45', delinquent: true },
    { local: '2026-03-01T00:30:45', adjusted: '2026-03-01T00:30:45', delinquent: false },
    { local: '2026-02-28T23:30:45', adjusted: '2026-02-28T23:30:45', delinquent: false }
  ])('uses the local date of a UTC timestamp for $local with delinquency $delinquent', ({ local, adjusted, delinquent }) => {
    const wrapper = mountBadge({
      subscriptionExpiresAt: new Date(local).toISOString(),
      subscriptionIsDelinquent: delinquent
    })
    expect(wrapper.get(expirySelector).text()).toBe(`Expires ${adjusted.slice(0, 10)}`)
    expect(wrapper.get(expirySelector).attributes('title')).toBe(localDateTime(new Date(adjusted)))
  })

  it.each([
    { planType: 'pro', days: 14 },
    { planType: 'chatgptpro', days: 14 },
    { planType: 'prolite', days: 14 },
    { planType: 'promax', days: 14 },
    { planType: 'plus', days: 30 }
  ])('adds $planType grace as $days fixed days from the adjusted date across DST', ({ planType, days }) => {
    const wrapper = mountBadge({
      planType,
      subscriptionExpiresAt: '2026-04-01T23:30:45',
      subscriptionIsDelinquent: true
    })
    const until = new Date(new Date('2026-03-01T23:30:45').getTime() + days * 24 * 60 * 60 * 1000)
    expect(wrapper.get(expirySelector).text()).toBe('Expires 2026-03-01')
    expect(wrapper.get(graceSelector).text()).toBe(`Grace ${formatDateLocalInput(until)}`)
    expect(wrapper.get(graceSelector).attributes('title')).toBe(localDateTime(until))
  })

  it('recomputes from the raw entitlement date when delinquency, expiry or plan changes', async () => {
    const wrapper = mountBadge({ subscriptionExpiresAt: '2026-11-28T12:34:56', subscriptionIsDelinquent: true })
    expect(wrapper.get(expirySelector).text()).toBe('Expires 2026-10-28')
    for (const subscriptionIsDelinquent of [false, null, undefined]) {
      await wrapper.setProps({ subscriptionIsDelinquent })
      expect(wrapper.get(expirySelector).text()).toBe('Expires 2026-11-28')
      expect(wrapper.get(graceSelector).text()).toBe('Grace 2026-12-12')
    }
    await wrapper.setProps({ subscriptionExpiresAt: '2026-12-28T12:34:56', subscriptionIsDelinquent: true })
    expect(wrapper.get(expirySelector).text()).toBe('Expires 2026-11-28')
    expect(wrapper.get(graceSelector).text()).toBe('Grace 2026-12-12')
    await wrapper.setProps({ planType: 'team' })
    expect(wrapper.find(graceSelector).exists()).toBe(false)
  })

  it.each([
    'free', 'basic', 'xbasic', 'business', 'team', 'unknown', 'go', 'enterprise',
    'self_serve_business_prolite', 'self_serve_business_usage_based', 'edu_pro', 'future_sku', '', undefined
  ])('does not show grace for %s', (planType) => {
    expect(mountBadge({ planType }).find(graceSelector).exists()).toBe(false)
  })

  it.each<AccountPlatform>(['anthropic', 'antigravity', 'grok', 'gemini'])('does not adjust expiry or apply grace to %s', (platform) => {
    const wrapper = mountBadge({ platform, subscriptionIsDelinquent: true })
    expect(wrapper.find(graceSelector).exists()).toBe(false)
    expect(wrapper.get(expirySelector).text()).toBe(`Expires ${formatDateLocalInput(new Date(expiration))}`)
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
    expect(grace.text()).toBe(`Grace ${formatDateLocalInput(new Date(until))}`)
    expect(grace.attributes('title')).toBe(localDateTime(new Date(until)))
  })

  it('renders the Chinese Plus grace date with the full local time only in its title', () => {
    const wrapper = mountBadge({ planType: 'plus' }, 'zh')
    const grace = wrapper.get(graceSelector)
    const until = new Date('2026-11-27T12:34:56Z')
    expect(wrapper.get(expirySelector).text()).toBe('到期 2026-10-28')
    expect(grace.text()).toBe(`宽限 ${formatDateLocalInput(until)}`)
    expect(grace.attributes('title')).toBe(localDateTime(until, 'zh'))
  })
})
