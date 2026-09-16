import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import OpsWorkerRuntimeCard from './OpsWorkerRuntimeCard.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

describe('OpsWorkerRuntimeCard', () => {
  it('renders registered worker names from process-local status', () => {
    const wrapper = mount(OpsWorkerRuntimeCard, {
      props: {
        loading: false,
        status: {
          scope: 'process',
          workers: [
            { Descriptor: { Name: 'account-expiry', Kind: 'periodic' }, Lifecycle: { State: 'running', UpdatedAt: '2026-09-16T00:00:00Z' } },
          ],
        },
      },
    })
    expect(wrapper.text()).toContain('account-expiry')
    expect(wrapper.text()).toContain('admin.ops.workerRuntime.title')
  })
})
