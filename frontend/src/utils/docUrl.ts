import { sanitizeUrl } from '@/utils/url'

export const DEFAULT_DOCUMENTATION_URL = '/guide'

/** Resolve an administrator-configured external docs URL or use the local guide. */
export function resolveDocumentationUrl(configuredUrl?: string | null): string {
  const sanitized = sanitizeUrl(configuredUrl || '')
  return sanitized || DEFAULT_DOCUMENTATION_URL
}
