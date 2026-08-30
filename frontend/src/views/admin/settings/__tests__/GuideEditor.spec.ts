import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import GuideEditor from '@/views/admin/settings/GuideEditor.vue'

const { getGuideSettings, updateGuideSettings, showError, showSuccess, showWarning } = vi.hoisted(() => ({
  getGuideSettings: vi.fn(),
  updateGuideSettings: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showWarning: vi.fn(),
}))

vi.mock('@/api', () => ({
  adminAPI: {
    settings: {
      getGuideSettings,
      updateGuideSettings,
      restoreGuideSettings: vi.fn(),
      resetGuideSettings: vi.fn(),
    },
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({ showError, showSuccess, showWarning }),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      locale: { value: 'zh-CN' },
      t: (key: string, params?: Record<string, string | number>) => {
        if (key.endsWith('currentVersion')) return `当前版本：第 ${params?.version} 版`
        if (key.endsWith('publishSuccess')) return '教程已发布'
        return key
      },
    }),
  }
})

describe('GuideEditor', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    getGuideSettings.mockResolvedValue({
      content: '## 当前教程\n\n旧内容',
      version: 3,
      updated_at: '2026-08-30T10:00:00Z',
      has_custom_content: true,
      revisions: [{
        version: 3,
        content: '## 当前教程\n\n旧内容',
        updated_at: '2026-08-30T10:00:00Z',
      }],
    })
    updateGuideSettings.mockResolvedValue({
      content: '## 新教程\n\n新内容',
      version: 4,
      updated_at: '2026-08-30T11:00:00Z',
      has_custom_content: true,
      revisions: [],
    })
  })

  it('loads, previews, and publishes Markdown with optimistic versioning', async () => {
    const wrapper = mount(GuideEditor, {
      global: {
        stubs: {
          ConfirmDialog: true,
          Icon: true,
        },
      },
    })
    await flushPromises()

    expect(wrapper.get<HTMLTextAreaElement>('[data-test="guide-markdown"]').element.value)
      .toContain('当前教程')
    expect(wrapper.get('[data-test="guide-preview"]').text()).toContain('旧内容')

    await wrapper.get('[data-test="guide-markdown"]').setValue('## 新教程\n\n新内容')
    expect(wrapper.get('[data-test="guide-preview"]').text()).toContain('新内容')
    await wrapper.get('[data-test="guide-save"]').trigger('click')
    await flushPromises()

    expect(updateGuideSettings).toHaveBeenCalledWith({
      content: '## 新教程\n\n新内容',
      expected_version: 3,
    })
    expect(showSuccess).toHaveBeenCalledWith('教程已发布')
  })
})
