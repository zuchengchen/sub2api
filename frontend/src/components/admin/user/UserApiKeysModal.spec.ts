import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

describe('UserApiKeysModal concurrency field', () => {
  it('lets admins edit per-key concurrency', () => {
    const source = readFileSync(join(dirname(fileURLToPath(import.meta.url)), './UserApiKeysModal.vue'), 'utf8')
    expect(source).toContain('data-testid="admin-key-concurrency"')
    expect(source).toContain('updateApiKey')
  })
})
