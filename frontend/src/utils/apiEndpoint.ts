/**
 * Resolves the API endpoint shown to users ("Use Key", CC Switch import, the
 * endpoint chip on /keys, batch-image guide).
 *
 * ## Why this module exists
 *
 * `api_base_url` is a single global admin setting, so a deployment reachable
 * under several domains (e.g. key66.vip and key66.cc.cd) advertised the same
 * hard-coded host no matter which one the user opened. Clearing the setting is
 * not a fix: it also hides the endpoint chip and drops the intended path suffix.
 *
 * When the admin enables `api_base_url_follow_host`, the endpoint follows the
 * browser's current origin while keeping the configured path suffix, so
 * `https://key66.vip/v1` becomes `https://key66.cc.cd/v1` on the other domain.
 *
 * Resolution must happen in the browser: the backend caches the injected
 * `window.__APP_CONFIG__` HTML globally without keying on Host
 * (backend/internal/web/html_cache.go), so a server-rendered value would leak
 * the first requested domain to every other one.
 */

export interface ResolveApiEndpointInput {
  /** Admin-configured `api_base_url`; may be empty, and may carry a path. */
  configured?: string | null
  /** Admin-configured `api_base_url_follow_host`. */
  followHost?: boolean | null
  /**
   * Origin to follow. Defaults to `window.location.origin`; pass explicitly in
   * tests or non-browser contexts.
   */
  currentOrigin?: string | null
}

function stripTrailingSlashes(value: string): string {
  return value.replace(/\/+$/, '')
}

function browserOrigin(): string {
  if (typeof window === 'undefined') return ''
  return stripTrailingSlashes(String(window.location?.origin || '').trim())
}

/**
 * Path + query + hash of the configured endpoint, so a configured
 * `https://key66.vip/v1` keeps `/v1` when it follows another host.
 */
function configuredSuffix(configured: string): string {
  try {
    const parsed = new URL(configured)
    const path = stripTrailingSlashes(parsed.pathname)
    return `${path}${parsed.search}${parsed.hash}`
  } catch {
    return ''
  }
}

/**
 * The endpoint to display, or `''` when nothing usable is available (callers
 * treat empty as "no endpoint to show").
 */
export function resolveApiEndpoint(input: ResolveApiEndpointInput): string {
  const configured = stripTrailingSlashes(String(input.configured || '').trim())
  const origin =
    input.currentOrigin === undefined
      ? browserOrigin()
      : stripTrailingSlashes(String(input.currentOrigin || '').trim())

  if (!input.followHost) {
    return configured || origin
  }
  if (!origin) {
    // No browser origin (SSR/tests): the configured value is the best guess.
    return configured
  }
  return `${origin}${configured ? configuredSuffix(configured) : ''}`
}

/** Convenience wrapper for the public-settings shape used across views. */
export function resolveApiEndpointFromSettings(
  settings?: {
    api_base_url?: string | null
    api_base_url_follow_host?: boolean | null
  } | null,
  currentOrigin?: string | null,
): string {
  return resolveApiEndpoint({
    configured: settings?.api_base_url,
    followHost: settings?.api_base_url_follow_host,
    ...(currentOrigin === undefined ? {} : { currentOrigin }),
  })
}
