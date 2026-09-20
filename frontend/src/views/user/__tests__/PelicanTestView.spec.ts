import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import PelicanTestView from '../PelicanTestView.vue'
import { pelicanTestsAPI } from '@/api/pelicanTests'

vi.mock('@/api/pelicanTests', () => ({
  pelicanTestsAPI: { list: vi.fn() }
}))
vi.mock('@/components/layout/AppLayout.vue', () => ({
  default: { template: '<div><slot /></div>' }
}))
vi.mock('@/components/common/LoadingSpinner.vue', () => ({
  default: { template: '<div>loading</div>' }
}))

const messages = {
  zh: {
    nav: { pelicanTest: '鹈鹕测试' },
    pelicanTest: {
      subtitle: '每小时自动',
      empty: '暂无测试结果',
      time: '测试时间',
      group: '测试分组',
      model: '模型',
      reasoning: '推理深度',
      htmlUnavailable: '无法安全显示这段 HTML',
      loadFailed: '加载失败'
    }
  }
}

function mountView() {
  const i18n = createI18n({ legacy: false, locale: 'zh', messages })
  return mount(PelicanTestView, { global: { plugins: [i18n] } })
}

describe('PelicanTestView', () => {
  it('renders HTML results with time, group, model and reasoning and has no run button', async () => {
    vi.mocked(pelicanTestsAPI.list).mockResolvedValue({
      items: [{
        id: 9,
        status: 'completed',
        html: '<html><body><svg viewBox="0 0 1 1"></svg></body></html>',
        model: 'gpt-6-astra',
        group_name: 'GPT-PRO',
        reasoning_effort: 'low',
        created_at: '2026-09-20T02:00:00Z',
        finished_at: '2026-09-20T02:00:12Z',
        duration_ms: 12000
      }],
      total: 1,
      page: 1,
      page_size: 12
    })
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.text()).toContain('GPT-PRO')
    expect(wrapper.text()).toContain('gpt-6-astra')
    expect(wrapper.text()).toContain('low')
    expect(wrapper.text()).toContain('pelicanTest.time')
    expect(wrapper.find('iframe').exists()).toBe(true)
    expect(wrapper.find('iframe').attributes('sandbox')).toBeDefined()
    expect(wrapper.text()).not.toContain('开始测试')
    expect(wrapper.text()).not.toContain('重新测试')
    expect(wrapper.html()).not.toMatch(/<button/i)
    wrapper.unmount()
  })
})
