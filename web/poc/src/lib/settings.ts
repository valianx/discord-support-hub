// Runtime settings — API key and hub base URL.
// The key is stored in sessionStorage only (cleared on tab close).
// It is NEVER hardcoded, committed, or sent anywhere except the hub API.

const SESSION_KEY = 'hub_api_key'
const BASE_URL_KEY = 'hub_base_url'

// VITE_HUB_BASE_URL lets operators pre-configure the hub origin at build time
// (e.g. for a deployed instance pointing at a remote hub). Falls back to '/'
// which routes via the Vite dev proxy in development.
const DEFAULT_BASE_URL = import.meta.env.VITE_HUB_BASE_URL || '/'

export function getApiKey(): string {
  return sessionStorage.getItem(SESSION_KEY) ?? ''
}

export function setApiKey(key: string): void {
  if (key.trim() === '') {
    sessionStorage.removeItem(SESSION_KEY)
  } else {
    sessionStorage.setItem(SESSION_KEY, key)
  }
}

export function clearApiKey(): void {
  sessionStorage.removeItem(SESSION_KEY)
}

export function getBaseUrl(): string {
  return sessionStorage.getItem(BASE_URL_KEY) ?? DEFAULT_BASE_URL
}

export function setBaseUrl(url: string): void {
  if (url.trim() === '' || url === DEFAULT_BASE_URL) {
    sessionStorage.removeItem(BASE_URL_KEY)
  } else {
    sessionStorage.setItem(BASE_URL_KEY, url)
  }
}

export function hasApiKey(): boolean {
  return getApiKey().length > 0
}
