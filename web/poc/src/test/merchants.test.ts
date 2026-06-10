import { describe, it, expect, vi, beforeEach } from 'vitest'
import {
  ApiError,
  registerMerchant,
  listMerchants,
  type ApiConfig,
} from '@/lib/api'

const config: ApiConfig = {
  baseUrl: 'http://localhost:8080',
}

const mockMerchant = {
  id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
  external_ref: 'acme-corp',
  name: 'Acme Corp',
  help_desk_url: null,
  is_active: true,
  created_at: '2026-06-09T00:00:00Z',
}

describe('registerMerchant', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('POSTs to /v1/merchants with JSON body and no client Authorization header', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 201,
      json: async () => mockMerchant,
    })
    vi.stubGlobal('fetch', mockFetch)

    const result = await registerMerchant(config, {
      external_ref: 'acme-corp',
      name: 'Acme Corp',
    })

    expect(mockFetch).toHaveBeenCalledOnce()
    const [url, options] = mockFetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('http://localhost:8080/v1/merchants')
    expect(options.method).toBe('POST')
    // Authorization is injected by the nginx proxy — the client must NOT send it.
    expect((options.headers as Record<string, string>)['Authorization']).toBeUndefined()
    expect((options.headers as Record<string, string>)['Content-Type']).toBe('application/json')
    expect(JSON.parse(options.body as string)).toEqual({
      external_ref: 'acme-corp',
      name: 'Acme Corp',
    })
    expect(result).toEqual(mockMerchant)
  })

  it('throws ApiError with status 409 on duplicate external_ref', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({
        code: 'conflict',
        message: 'external_ref already exists',
        details: null,
      }),
    })
    vi.stubGlobal('fetch', mockFetch)

    await expect(
      registerMerchant(config, { external_ref: 'acme-corp', name: 'Acme Corp' })
    ).rejects.toThrow(ApiError)

    try {
      await registerMerchant(config, { external_ref: 'acme-corp', name: 'Acme Corp' })
    } catch (err) {
      expect(err).toBeInstanceOf(ApiError)
      const apiErr = err as ApiError
      expect(apiErr.status).toBe(409)
      expect(apiErr.code).toBe('conflict')
    }
  })

  it('throws ApiError with status 400 on validation failure', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 400,
      json: async () => ({
        code: 'validation_error',
        message: 'name is required',
        details: null,
      }),
    })
    vi.stubGlobal('fetch', mockFetch)

    try {
      await registerMerchant(config, { external_ref: 'x', name: '' })
    } catch (err) {
      expect(err).toBeInstanceOf(ApiError)
      const apiErr = err as ApiError
      expect(apiErr.status).toBe(400)
      expect(apiErr.code).toBe('validation_error')
    }
  })

  it('sends Idempotency-Key header when provided', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 201,
      json: async () => mockMerchant,
    })
    vi.stubGlobal('fetch', mockFetch)

    await registerMerchant(
      config,
      { external_ref: 'acme-corp', name: 'Acme Corp' },
      'idem-key-123'
    )

    const [, options] = mockFetch.mock.calls[0] as [string, RequestInit]
    expect((options.headers as Record<string, string>)['Idempotency-Key']).toBe('idem-key-123')
  })
})

describe('listMerchants', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('GETs /v1/merchants with no params by default', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ items: [mockMerchant], next_cursor: null }),
    })
    vi.stubGlobal('fetch', mockFetch)

    const result = await listMerchants(config)

    const [url] = mockFetch.mock.calls[0] as [string]
    expect(url).toBe('http://localhost:8080/v1/merchants')
    expect(result.items).toHaveLength(1)
    expect(result.items[0].external_ref).toBe('acme-corp')
  })

  it('appends is_active query param when provided', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ items: [], next_cursor: null }),
    })
    vi.stubGlobal('fetch', mockFetch)

    await listMerchants(config, { is_active: true })

    const [url] = mockFetch.mock.calls[0] as [string]
    expect(url).toContain('is_active=true')
  })

  it('appends cursor param when provided', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ items: [], next_cursor: null }),
    })
    vi.stubGlobal('fetch', mockFetch)

    await listMerchants(config, { cursor: 'next-page-token' })

    const [url] = mockFetch.mock.calls[0] as [string]
    expect(url).toContain('cursor=next-page-token')
  })
})
