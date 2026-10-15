import type { User } from '@/types'

/** 自动升级阈值（与后端 service.VipBalanceThreshold 同源）：总余额达到该值即升级。 */
export const VIP_BALANCE_THRESHOLD = 100

/** 展示用冻结金额：仅批量图片等在途暂扣。SVIP 无保证金。 */
export function displayFrozenBalance(user: Pick<User, 'balance' | 'frozen_balance' | 'is_vip'> | null | undefined): number {
  return Number(user?.frozen_balance || 0)
}

/** 展示用可用余额：等于总余额。图片暂扣仍单独显示在冻结金额里。 */
export function displayAvailableBalance(user: Pick<User, 'balance' | 'frozen_balance' | 'is_vip'> | null | undefined): number {
  return Number(user?.balance || 0)
}
