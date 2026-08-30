import { describe, expect, it } from 'vitest'
import { DEFAULT_DOCUMENTATION_URL, resolveDocumentationUrl } from '@/utils/docUrl'

describe('resolveDocumentationUrl', () => {
  it('uses the same-host guide when doc_url is absent or unsafe', () => {
    expect(resolveDocumentationUrl()).toBe('/guide')
    expect(resolveDocumentationUrl('  ')).toBe('/guide')
    expect(resolveDocumentationUrl('javascript:alert(1)')).toBe('/guide')
    expect(resolveDocumentationUrl('//other.example/guide')).toBe('/guide')
    expect(DEFAULT_DOCUMENTATION_URL).toBe('/guide')
  })

  it('keeps a valid configured external documentation URL', () => {
    expect(resolveDocumentationUrl('https://docs.example.test/help?q=1')).toBe(
      'https://docs.example.test/help?q=1'
    )
  })
})
