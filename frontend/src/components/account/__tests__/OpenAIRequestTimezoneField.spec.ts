import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import OpenAIRequestTimezoneField from '@/components/account/OpenAIRequestTimezoneField.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}|${JSON.stringify(params)}` : key
    })
  }
})

const mountField = (props: Record<string, unknown> = {}) =>
  mount(OpenAIRequestTimezoneField, { props })

describe('OpenAIRequestTimezoneField', () => {
  it('reports manual override as the source when a timezone is set', async () => {
    const wrapper = mountField({ proxyTimezone: 'Asia/Shanghai' })
    await wrapper.find('[data-testid="openai-request-timezone"]').setValue('Europe/Berlin')
    const hint = wrapper.find('[data-testid="openai-request-timezone-source"]')
    expect(hint.text()).toContain('Europe/Berlin')
    expect(hint.text()).toContain('requestTimezoneSourceOverride')
  })

  it('reports the proxy egress timezone when no override is set', () => {
    const wrapper = mountField({ proxyTimezone: 'Asia/Shanghai' })
    const hint = wrapper.find('[data-testid="openai-request-timezone-source"]')
    expect(hint.text()).toContain('Asia/Shanghai')
    expect(hint.text()).toContain('requestTimezoneSourceProxy')
  })

  it('reports the direct egress timezone when no proxy timezone is known', () => {
    const wrapper = mountField({ directTimezone: 'America/New_York' })
    const hint = wrapper.find('[data-testid="openai-request-timezone-source"]')
    expect(hint.text()).toContain('America/New_York')
    expect(hint.text()).toContain('requestTimezoneSourceDirect')
  })

  it('marks the fallback default when nothing else resolves', () => {
    const wrapper = mountField()
    const hint = wrapper.find('[data-testid="openai-request-timezone-source"]')
    expect(hint.text()).toContain('America/Los_Angeles')
    expect(hint.text()).toContain('requestTimezoneSourceFallback')
  })
})
