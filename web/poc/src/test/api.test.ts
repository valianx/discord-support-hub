import { describe, it, expect, vi, beforeEach } from 'vitest'
import {
  ApiError,
  listSpaces,
  provisionSpace,
  expelCollaborator,
  type ApiConfig,
} from '@/lib/api'

const config: ApiConfig = {
  baseUrl: 'http://localhost:8080',
  apiKey: 'test-key-abc123',
}

describe('API client — Authorization header', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('sends Bearer token in every request', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ items: [], next_cursor: null }),
    })
    vi.stubGlobal('fetch', mockFetch)

    await listSpaces(config)

    expect(mockFetch).toHaveBeenCalledOnce()
    const [, options] = mockFetch.mock.calls[0] as [string, RequestInit]
    expect((options.headers as Record<string, string>)['Authorization']).toBe(
      `Bearer ${config.apiKey}`
    )
  })

  it('includes Content-Type for mutating requests', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({
        job: {
          id: 'job-1',
          kind: 'provision_space',
          status: 'pending',
          retry_count: 0,
          error: null,
          created_at: new Date().toISOString(),
          completed_at: null,
          space_id: null,
          merchant_id: null,
          user_id: null,
        },
      }),
    })
    vi.stubGlobal('fetch', mockFetch)

    await provisionSpace(config, 'merchant-1', { name: 'test-space' })

    const [, options] = mockFetch.mock.calls[0] as [string, RequestInit]
    expect((options.headers as Record<string, string>)['Content-Type']).toBe(
      'application/json'
    )
  })

  it('builds the correct URL path for expel with scope query param', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({
        job: {
          id: 'job-2',
          kind: 'expel',
          status: 'pending',
          retry_count: 0,
          error: null,
          created_at: new Date().toISOString(),
          completed_at: null,
          space_id: 'space-1',
          merchant_id: null,
          user_id: 'user-1',
        },
      }),
    })
    vi.stubGlobal('fetch', mockFetch)

    await expelCollaborator(config, 'space-1', 'user-1', 'server')

    const [url] = mockFetch.mock.calls[0] as [string]
    expect(url).toContain('/v1/channels/space-1/collaborators/user-1')
    expect(url).toContain('scope=server')
  })
})

describe('API client — error handling', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('throws ApiError with code and message from hub error shape', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 404,
      json: async () => ({
        code: 'not_found',
        message: 'Space does not exist',
        details: null,
      }),
    })
    vi.stubGlobal('fetch', mockFetch)

    await expect(listSpaces(config)).rejects.toThrow(ApiError)

    try {
      await listSpaces(config)
    } catch (err) {
      expect(err).toBeInstanceOf(ApiError)
      const apiErr = err as ApiError
      expect(apiErr.code).toBe('not_found')
      expect(apiErr.message).toBe('Space does not exist')
      expect(apiErr.status).toBe(404)
    }
  })

  it('throws ApiError with fallback code on non-JSON error response', async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 503,
      json: async () => { throw new Error('not json') },
    })
    vi.stubGlobal('fetch', mockFetch)

    try {
      await listSpaces(config)
    } catch (err) {
      expect(err).toBeInstanceOf(ApiError)
      const apiErr = err as ApiError
      expect(apiErr.code).toBe('unknown_error')
      expect(apiErr.status).toBe(503)
    }
  })
})
