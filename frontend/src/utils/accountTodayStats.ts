import type { Account } from '@/types'
import { isMultiProtocolApiKeyPlatform } from '@/components/account/credentialsBuilder'

/** Matches AccountUsageCell's non-window branch, which embeds today's stats. */
export function accountUsageShowsTodayStats(account: Pick<Account, 'platform' | 'type'>): boolean {
  return !isMultiProtocolApiKeyPlatform(account.platform) &&
    !(account.platform === 'openai' && account.type === 'apikey') &&
    account.type !== 'oauth' && account.type !== 'setup-token'
}
