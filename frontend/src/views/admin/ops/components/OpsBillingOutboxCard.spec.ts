import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import OpsBillingOutboxCard from './OpsBillingOutboxCard.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('@/utils/format', () => ({
  formatNumber: (value: number) => String(value),
}))

describe('OpsBillingOutboxCard', () => {
  it('renders billing outbox health metrics', () => {
    const wrapper = mount(OpsBillingOutboxCard, {
      props: {
        loading: false,
        health: {
          running: true,
          processed: 3,
          failures: 0,
          pending: 1,
          processing: 0,
          terminal: 0,
          oldest_lag: 0,
          max_attempts: 10,
          circuit_open: false,
          permanent_failures: 0,
          backlogged_rounds: 0,
          round_timeouts: 0,
        },
      },
    })
    expect(wrapper.text()).toContain('admin.ops.billingOutbox.title')
    expect(wrapper.text()).toContain('admin.ops.billingOutbox.running')
  })
})
