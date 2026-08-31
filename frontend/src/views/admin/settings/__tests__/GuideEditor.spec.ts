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
      content: '## 第一章\n\n旧内容\n\n## 第二章\n\n第二章内容',
      chapters: [
        { slug: 'alpha', title: '第一章', content: '## 第一章\n\n旧内容' },
        { slug: 'beta', title: '第二章', content: '## 第二章\n\n第二章内容' },
      ],
      version: 3,
      updated_at: '2026-08-30T10:00:00Z',
      has_custom_content: true,
      revisions: [{
        version: 3,
        content: '## 第一章\n\n旧内容',
        updated_at: '2026-08-30T10:00:00Z',
      }],
    })
    updateGuideSettings.mockResolvedValue({
      content: '## 第一章\n\n新内容\n\n## 第二章\n\n第二章内容',
      chapters: [
        { slug: 'alpha', title: '第一章', content: '## 第一章\n\n新内容' },
        { slug: 'beta', title: '第二章', content: '## 第二章\n\n第二章内容' },
      ],
      version: 4,
      updated_at: '2026-08-30T11:00:00Z',
      has_custom_content: true,
      revisions: [],
    })
  })

  function mountEditor() {
    return mount(GuideEditor, {
      global: { stubs: { ConfirmDialog: true, Icon: true } },
    })
  }

  it('edits one chapter at a time and publishes every chapter with optimistic versioning', async () => {
    const wrapper = mountEditor()
    await flushPromises()

    // The first chapter is selected by default; the editor shows only its body.
    const textarea = wrapper.get<HTMLTextAreaElement>('[data-test="guide-markdown"]')
    expect(textarea.element.value).toBe('## 第一章\n\n旧内容')
    expect(textarea.element.value).not.toContain('第二章内容')

    // The preview renders the whole guide so ordering stays visible.
    expect(wrapper.get('[data-test="guide-preview"]').text()).toContain('旧内容')
    expect(wrapper.get('[data-test="guide-preview"]').text()).toContain('第二章内容')

    await wrapper.get('[data-test="guide-chapter-beta"]').trigger('click')
    expect(wrapper.get<HTMLTextAreaElement>('[data-test="guide-markdown"]').element.value)
      .toBe('## 第二章\n\n第二章内容')

    await wrapper.get('[data-test="guide-chapter-alpha"]').trigger('click')
    await wrapper.get('[data-test="guide-markdown"]').setValue('## 第一章\n\n新内容')
    await wrapper.get('[data-test="guide-save"]').trigger('click')
    await flushPromises()

    // Saving one chapter still publishes the full list, so the untouched
    // chapter must be sent back verbatim rather than dropped.
    expect(updateGuideSettings).toHaveBeenCalledWith({
      chapters: [
        { slug: 'alpha', title: '第一章', content: '## 第一章\n\n新内容' },
        { slug: 'beta', title: '第二章', content: '## 第二章\n\n第二章内容' },
      ],
      expected_version: 3,
    })
    expect(showSuccess).toHaveBeenCalledWith('教程已发布')
  })

  it('adds, reorders, and removes chapters without touching other slugs', async () => {
    const wrapper = mountEditor()
    await flushPromises()

    await wrapper.get('[data-test="guide-chapter-new-title"]').setValue('Extra Notes')
    await wrapper.get('[data-test="guide-chapter-add"]').trigger('click')

    // A new chapter gets a slug derived from its title and a seeded heading.
    expect(wrapper.find('[data-test="guide-chapter-extra-notes"]').exists()).toBe(true)
    expect(wrapper.get<HTMLTextAreaElement>('[data-test="guide-markdown"]').element.value)
      .toBe('## Extra Notes\n\n')

    await wrapper.get('[data-test="guide-chapter-up-extra-notes"]').trigger('click')
    await wrapper.get('[data-test="guide-save"]').trigger('click')
    await flushPromises()

    const published = updateGuideSettings.mock.calls[0][0] as {
      chapters: Array<{ slug: string }>
    }
    expect(published.chapters.map((chapter) => chapter.slug)).toEqual(['alpha', 'extra-notes', 'beta'])
  })

  it('renders history for chapter revisions that carry no whole-document content', async () => {
    // A revision published by the chapter editor has no `content` field; reading
    // it unguarded used to throw and blank the whole editor.
    getGuideSettings.mockResolvedValue({
      chapters: [{ slug: 'alpha', title: '第一章', content: '## 第一章\n\n内容' }],
      version: 5,
      updated_at: '2026-08-30T10:00:00Z',
      has_custom_content: true,
      revisions: [
        {
          version: 5,
          chapters: [{ slug: 'alpha', title: '第一章', content: '## 第一章\n\n内容' }],
          updated_at: '2026-08-30T10:00:00Z',
        },
        { version: 4, updated_at: '2026-08-30T09:00:00Z' },
      ],
    })

    const wrapper = mountEditor()
    await flushPromises()

    expect(wrapper.get('[data-test="guide-chapter-list"]').findAll('li')).toHaveLength(1)
    expect(wrapper.get<HTMLTextAreaElement>('[data-test="guide-markdown"]').element.value)
      .toBe('## 第一章\n\n内容')
    // Both revisions render a row: the chapter-based one as database content and
    // the empty one as the bundled guide.
    const historyRows = wrapper.findAll('[data-test="guide-revision-row"]')
    expect(historyRows).toHaveLength(2)
    expect(historyRows[0].text()).toContain('databaseContent')
    expect(historyRows[1].text()).toContain('bundledContent')
  })

  it('marks the guide dirty only after an actual edit', async () => {
    const wrapper = mountEditor()
    await flushPromises()

    expect(wrapper.get('[data-test="guide-save"]').attributes('disabled')).toBeDefined()

    await wrapper.get('[data-test="guide-markdown"]').setValue('## 第一章\n\n改过了')
    expect(wrapper.get('[data-test="guide-save"]').attributes('disabled')).toBeUndefined()
  })
})
