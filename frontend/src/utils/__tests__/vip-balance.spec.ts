import { describe, expect, it } from 'vitest'

import { displayAvailableBalance } from '../vip-balance'

describe('vip balance display helpers', () => {
  it('shows the full ledger balance for every user', () => {
    expect(displayAvailableBalance({ balance: 50, frozen_balance: 5, is_vip: false })).toBe(50)
    expect(displayAvailableBalance({ balance: 139, frozen_balance: 0, is_vip: true })).toBe(139)
    expect(displayAvailableBalance({ balance: 139, frozen_balance: 7, is_vip: true })).toBe(139)
    expect(displayAvailableBalance({ balance: 10 })).toBe(10)
  })
})
