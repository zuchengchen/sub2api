import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

describe('KeysView concurrency field', () => {
  it('exposes an editable concurrency input defaulting to unlimited', () => {
    const source = readFileSync(join(dirname(fileURLToPath(import.meta.url)), '../KeysView.vue'), 'utf8')
    expect(source).toContain('data-testid="key-concurrency"')
    expect(source).toContain('concurrency: 0')
    expect(source).toContain('keys.concurrencyHint')
  })
})
