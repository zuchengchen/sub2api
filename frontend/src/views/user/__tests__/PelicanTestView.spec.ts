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
      prompt: '原始提示词',
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
      items: [
        {
          id: 10,
          status: 'completed',
          html: '<html><body><svg id="latest" viewBox="0 0 1 1"></svg></body></html>',
          prompt: '创建一个HTML，内容是SVG绘制一个火烈鸟骑自行车的2D动画',
          model: 'gpt-6-astra',
          group_name: 'GPT-PRO',
          reasoning_effort: 'low',
          created_at: '2026-09-20T02:50:00Z',
          finished_at: '2026-09-20T02:50:12Z',
          duration_ms: 12000
        },
        {
          id: 9,
          status: 'completed',
          html: '<html><body><svg id="older" viewBox="0 0 1 1"></svg></body></html>',
          prompt: '创建一个HTML，内容是SVG绘制一个鹈鹕骑自行车的2D动画',
          model: 'gpt-6-astra',
          group_name: 'GPT-PRO',
          reasoning_effort: 'low',
          created_at: '2026-09-20T02:40:00Z',
          finished_at: '2026-09-20T02:40:12Z',
          duration_ms: 11000
        }
      ],
      total: 2,
      page: 1,
      page_size: 12
    })
    const wrapper = mountView()
    await flushPromises()
    expect(pelicanTestsAPI.list).toHaveBeenCalledWith(1, 32)
    expect(wrapper.findAll('h1')).toHaveLength(1)
    expect(wrapper.get('h1').classes()).toContain('lg:hidden')
    expect(wrapper.findAll('iframe')).toHaveLength(1)
    expect(wrapper.find('iframe').attributes('srcdoc')).toContain('id="latest"')
    expect(wrapper.text()).toContain('火烈鸟')
    const buttons = wrapper.findAll('button')
    expect(buttons).toHaveLength(2)
    const timeColumn = buttons[0].element.parentElement
    expect(timeColumn).toBeTruthy()
    expect(timeColumn?.parentElement).toBe(wrapper.find('article').element.parentElement)
    expect(timeColumn?.className).toContain('flex-col')
    await buttons[1].trigger('click')
    expect(wrapper.find('iframe').attributes('srcdoc')).toContain('id="older"')
    expect(wrapper.text()).toContain('鹈鹕')
    expect(wrapper.text()).not.toContain('开始测试')
    expect(wrapper.text()).not.toContain('重新测试')
    wrapper.unmount()
  })
})
