import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import TiboRouteSpark from '../TiboRouteSpark.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

describe('TiboRouteSpark', () => {
  it('renders astra, ten dots, and the health verdict', () => {
    const wrapper = mount(TiboRouteSpark, {
      props: {
        model: 'gpt-6-astra',
        verdict: 'healthy',
        samples: [
          { verdict: 'healthy' },
          { verdict: 'degraded' },
          { verdict: 'unknown' },
        ],
      },
    })
    expect(wrapper.text()).toContain('astra')
    expect(wrapper.text()).toContain('admin.accounts.openai.codexTiboRouteVerdict.healthy')
    const dots = wrapper.findAll('[data-testid="tibo-dot"]')
    expect(dots).toHaveLength(10)
    expect(dots.map(dot => dot.attributes('data-verdict'))).toEqual([
      'empty', 'empty', 'empty', 'empty', 'empty', 'empty', 'empty',
      'healthy', 'degraded', 'unknown',
    ])
    expect(dots[7].classes().join(' ')).toContain('bg-emerald-500')
    expect(dots[8].classes().join(' ')).toContain('bg-red-500')
    expect(dots[9].classes().join(' ')).toContain('bg-red-500')
  })
})
