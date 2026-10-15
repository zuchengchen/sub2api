import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

const dir = dirname(fileURLToPath(import.meta.url))
const headerSource = readFileSync(resolve(dir, '../AppHeader.vue'), 'utf8')
const homeViewSource = readFileSync(resolve(dir, '../../../views/HomeView.vue'), 'utf8')
const keyUsageViewSource = readFileSync(resolve(dir, '../../../views/KeyUsageView.vue'), 'utf8')

describe('doc_url sanitization', () => {
  it('AppHeader uses the shared documentation URL resolver', () => {
    expect(headerSource).toContain("import { resolveDocumentationUrl } from '@/utils/docUrl'")
    expect(headerSource).toContain('resolveDocumentationUrl(appStore.docUrl)')
  })

  it('AppHeader does not show a frozen-balance breakdown', () => {
    expect(headerSource).not.toContain('frozenBalance')
    expect(headerSource).not.toContain('displayFrozenBalance')
    expect(headerSource).not.toContain('冻结金额')
  })

  it('HomeView uses the shared documentation URL resolver', () => {
    expect(homeViewSource).toContain("import { resolveDocumentationUrl } from '@/utils/docUrl'")
    expect(homeViewSource).toContain(
      'resolveDocumentationUrl(appStore.cachedPublicSettings?.doc_url ?? appStore.docUrl)'
    )
  })

  it('KeyUsageView uses the shared documentation URL resolver', () => {
    expect(keyUsageViewSource).toContain("import { resolveDocumentationUrl } from '@/utils/docUrl'")
    expect(keyUsageViewSource).toContain(
      'resolveDocumentationUrl(appStore.cachedPublicSettings?.doc_url ?? appStore.docUrl)'
    )
  })
})
