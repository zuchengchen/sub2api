import { afterEach, describe, expect, it } from 'vitest'

import { getPersistedPageSize } from '@/composables/usePersistedPageSize'

describe('usePersistedPageSize', () => {
  afterEach(() => {
    localStorage.clear()
    delete window.__APP_CONFIG__
  })

  it('uses the system table default instead of stale localStorage state', () => {
    window.__APP_CONFIG__ = {
      table_default_page_size: 1000,
      table_page_size_options: [20, 50, 1000]
    } as any
    localStorage.setItem('table-page-size', '50')
    localStorage.setItem('table-page-size-source', 'user')

    expect(getPersistedPageSize()).toBe(1000)
    expect(getPersistedPageSize(50)).toBe(1000)
  })

  it('uses the page default with the built-in site configuration', () => {
    window.__APP_CONFIG__ = {
      table_default_page_size: 20,
      table_page_size_options: [20, 50, 1000]
    } as any
    localStorage.setItem('table-page-size', '1000')

    expect(getPersistedPageSize()).toBe(20)
    expect(getPersistedPageSize(50)).toBe(50)
  })

  it('keeps a saved page size when no site default is injected', () => {
    localStorage.setItem('table-page-size', '20')
    expect(getPersistedPageSize(50)).toBe(20)

    localStorage.setItem('table-page-size', 'invalid')
    expect(getPersistedPageSize(50)).toBe(50)
  })
})
