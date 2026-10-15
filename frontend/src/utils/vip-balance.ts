import type { User } from '@/types'

/** 自动升级阈值（与后端 service.VipBalanceThreshold 同源）：总余额达到该值即升级。 */
export const VIP_BALANCE_THRESHOLD = 100

/** 展示用余额：等于总余额。SVIP 无保证金，顶栏不再单独显示冻结金额。 */
export function displayAvailableBalance(user: Pick<User, 'balance'> | null | undefined): number {
  return Number(user?.balance || 0)
}
