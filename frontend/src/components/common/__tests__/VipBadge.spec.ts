import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import VipBadge from '../VipBadge.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

describe('VipBadge', () => {
  it('renders the default label from i18n with the crown icon and shine layer', () => {
    const wrapper = mount(VipBadge)

    expect(wrapper.find('[data-testid="vip-badge"]').exists()).toBe(true)
    expect(wrapper.find('.vip-badge-label').text()).toBe('common.vipBadge')
    expect(wrapper.find('svg').exists()).toBe(true)
    expect(wrapper.find('.vip-badge-shine').exists()).toBe(true)
    expect(wrapper.attributes('title')).toBe('common.vipBadgeTitle')
  })

  it('applies the requested size classes and custom label', () => {
    const wrapper = mount(VipBadge, {
      props: { size: 'md', label: 'SVIP' },
    })

    const root = wrapper.find('[data-testid="vip-badge"]')
    expect(root.classes()).toContain('text-xs')
    expect(wrapper.find('.vip-badge-label').text()).toBe('SVIP')
  })
})
