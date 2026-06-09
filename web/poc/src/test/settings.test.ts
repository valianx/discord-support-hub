import { describe, it, expect, beforeEach } from 'vitest'
import {
  getApiKey,
  setApiKey,
  clearApiKey,
  getBaseUrl,
  setBaseUrl,
  hasApiKey,
} from '@/lib/settings'

// jsdom provides sessionStorage in the test environment.

beforeEach(() => {
  sessionStorage.clear()
})

describe('settings — API key', () => {
  it('returns empty string when no key is stored', () => {
    expect(getApiKey()).toBe('')
  })

  it('persists the key in sessionStorage', () => {
    setApiKey('my-secret-key')
    expect(getApiKey()).toBe('my-secret-key')
    expect(sessionStorage.getItem('hub_api_key')).toBe('my-secret-key')
  })

  it('hasApiKey returns false when empty', () => {
    expect(hasApiKey()).toBe(false)
  })

  it('hasApiKey returns true after setting a key', () => {
    setApiKey('some-key')
    expect(hasApiKey()).toBe(true)
  })

  it('clearApiKey removes the key', () => {
    setApiKey('my-key')
    clearApiKey()
    expect(getApiKey()).toBe('')
    expect(hasApiKey()).toBe(false)
  })

  it('setApiKey with empty string removes the key', () => {
    setApiKey('my-key')
    setApiKey('')
    expect(getApiKey()).toBe('')
  })

  it('setApiKey with whitespace-only removes the key', () => {
    setApiKey('my-key')
    setApiKey('   ')
    expect(getApiKey()).toBe('')
  })
})

describe('settings — base URL', () => {
  it('returns "/" as default when nothing is stored', () => {
    expect(getBaseUrl()).toBe('/')
  })

  it('persists a custom base URL', () => {
    setBaseUrl('http://hub.example.com')
    expect(getBaseUrl()).toBe('http://hub.example.com')
  })

  it('setting empty string reverts to default "/"', () => {
    setBaseUrl('http://hub.example.com')
    setBaseUrl('')
    expect(getBaseUrl()).toBe('/')
  })

  it('setting "/" explicitly removes stored value and returns default', () => {
    setBaseUrl('http://hub.example.com')
    setBaseUrl('/')
    expect(getBaseUrl()).toBe('/')
  })
})
