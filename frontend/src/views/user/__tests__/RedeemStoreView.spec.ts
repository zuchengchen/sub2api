import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import RedeemStoreView from '../RedeemStoreView.vue'

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

describe('RedeemStoreView', () => {
  it('embeds the configured store and exposes a safe new-tab fallback', () => {
    const wrapper = mount(RedeemStoreView, {
      global: {
        stubs: {
          AppLayout: { template: '<main><slot /></main>' },
          Icon: { template: '<span data-testid="icon" />' },
        },
      },
    })

    const iframe = wrapper.get('iframe')
    expect(iframe.attributes('src')).toBe('https://catfk.com/shop/IGUZN6L4')
    expect(iframe.attributes('title')).toBe('nav.buyRedeemCode')
    expect(iframe.attributes('allow')).toBe('clipboard-write; payment')
    expect(iframe.attributes('referrerpolicy')).toBe('strict-origin-when-cross-origin')

    const fallback = wrapper.get('a')
    expect(fallback.attributes('href')).toBe('https://catfk.com/shop/IGUZN6L4')
    expect(fallback.attributes('target')).toBe('_blank')
    expect(fallback.attributes('rel')).toBe('noopener noreferrer')
  })
})
