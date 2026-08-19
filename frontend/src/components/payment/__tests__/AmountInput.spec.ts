import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import AmountInput from '../AmountInput.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

describe('AmountInput quick recharge amounts', () => {
  it('renders the six default tiers in two rows of three', () => {
    const wrapper = mount(AmountInput, {
      props: { modelValue: null },
    })

    const grid = wrapper.find('.grid')
    expect(grid.classes()).toContain('grid-cols-3')
    expect(grid.findAll('button').map(button => button.text())).toEqual([
      '10',
      '20',
      '30',
      '50',
      '100',
      '200',
    ])
  })
})
