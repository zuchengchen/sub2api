import { DEFAULT_TABLE_PAGE_SIZE, getConfiguredTableDefaultPageSize, normalizeTablePageSize } from '@/utils/tablePreferences'

const STORAGE_KEY = 'table-page-size'

export function getPersistedPageSize(pageDefault = DEFAULT_TABLE_PAGE_SIZE): number {
  if (typeof window !== 'undefined' && window.__APP_CONFIG__?.table_default_page_size !== undefined) {
    const configured = getConfiguredTableDefaultPageSize()
    return normalizeTablePageSize(configured === DEFAULT_TABLE_PAGE_SIZE ? pageDefault : configured)
  }

  if (typeof window !== 'undefined') {
    try {
      const stored = window.localStorage.getItem(STORAGE_KEY)
      if (stored !== null) {
        return normalizeTablePageSize(stored, pageDefault)
      }
    } catch (error) {
      console.warn('Failed to read persisted page size:', error)
    }
  }
  return normalizeTablePageSize(pageDefault)
}

export function setPersistedPageSize(size: number): void {
  if (typeof window === 'undefined') return
  try {
    window.localStorage.setItem(STORAGE_KEY, String(size))
  } catch (error) {
    console.warn('Failed to persist page size:', error)
  }
}
