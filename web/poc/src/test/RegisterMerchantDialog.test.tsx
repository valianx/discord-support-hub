import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { RegisterMerchantDialog } from '@/components/RegisterMerchantDialog'
import type { ApiConfig, Merchant } from '@/lib/api'

// Mock the api module so tests control every server response.
vi.mock('@/lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/api')>()
  return {
    ...actual,
    registerMerchant: vi.fn(),
  }
})

// Spy on sonner's toast so we can assert on calls without needing the Toaster
// DOM component — Sonner's toast container is not part of isolated renders.
vi.mock('sonner', async (importOriginal) => {
  const actual = await importOriginal<typeof import('sonner')>()
  return {
    ...actual,
    toast: {
      ...actual.toast,
      success: vi.fn(),
      error: vi.fn(),
    },
  }
})

import { registerMerchant, ApiError } from '@/lib/api'
import { toast } from 'sonner'

const mockRegisterMerchant = vi.mocked(registerMerchant)
const mockToastError = vi.mocked(toast.error)

const config: ApiConfig = {
  baseUrl: 'http://localhost:8080',
  apiKey: 'test-key-abc123',
}

const createdMerchant: Merchant = {
  id: 'bbbbbbbb-0000-0000-0000-000000000001',
  external_ref: 'acme-corp',
  name: 'Acme Corp',
  help_desk_url: null,
  invite_link: null,
  invite_link_set_at: null,
  is_active: true,
  created_at: '2026-06-09T00:00:00Z',
}

async function openDialog() {
  await userEvent.click(screen.getByRole('button', { name: /register merchant/i }))
  await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument())
}

async function fillAndSubmit(externalRef: string, name: string) {
  await userEvent.type(screen.getByLabelText(/external ref/i), externalRef)
  await userEvent.type(screen.getByLabelText(/name \(required\)/i), name)
  await userEvent.click(screen.getByRole('button', { name: /^register$/i }))
}

beforeEach(() => {
  vi.clearAllMocks()
  mockToastError.mockReset()
})

describe('RegisterMerchantDialog', () => {
  describe('success path', () => {
    it('shows the new merchant id in a read-only input after registration', async () => {
      mockRegisterMerchant.mockResolvedValue(createdMerchant)

      render(
        <RegisterMerchantDialog
          apiConfig={config}
          onRegistered={() => {}}
        />
      )

      await openDialog()
      await fillAndSubmit('acme-corp', 'Acme Corp')

      await waitFor(() => {
        // The success view renders the UUID in a read-only input.
        const idInput = screen.getByDisplayValue(createdMerchant.id)
        expect(idInput).toBeInTheDocument()
        expect(idInput).toHaveAttribute('readonly')
      })
    })

    it('shows a copy button next to the merchant id', async () => {
      mockRegisterMerchant.mockResolvedValue(createdMerchant)

      render(
        <RegisterMerchantDialog
          apiConfig={config}
          onRegistered={() => {}}
        />
      )

      await openDialog()
      await fillAndSubmit('acme-corp', 'Acme Corp')

      await waitFor(() => {
        expect(screen.getByTitle(/copy merchant id/i)).toBeInTheDocument()
      })
    })

    it('calls onRegistered with the returned merchant', async () => {
      mockRegisterMerchant.mockResolvedValue(createdMerchant)
      const onRegistered = vi.fn()

      render(
        <RegisterMerchantDialog
          apiConfig={config}
          onRegistered={onRegistered}
        />
      )

      await openDialog()
      await fillAndSubmit('acme-corp', 'Acme Corp')

      await waitFor(() => {
        expect(onRegistered).toHaveBeenCalledWith(createdMerchant)
      })
    })
  })

  describe('409 conflict', () => {
    it('surfaces an "already exists" toast when the hub returns 409', async () => {
      mockRegisterMerchant.mockRejectedValue(
        new ApiError(409, { code: 'conflict', message: 'external_ref already exists' })
      )

      render(
        <RegisterMerchantDialog
          apiConfig={config}
          onRegistered={() => {}}
        />
      )

      await openDialog()
      await fillAndSubmit('acme-corp', 'Acme Corp')

      await waitFor(() => {
        expect(mockToastError).toHaveBeenCalledWith(
          expect.stringMatching(/external_ref already exists/i)
        )
      })
    })
  })

  describe('400 validation error', () => {
    it('surfaces a validation toast when the hub returns 400', async () => {
      mockRegisterMerchant.mockRejectedValue(
        new ApiError(400, { code: 'validation_error', message: 'name is required' })
      )

      render(
        <RegisterMerchantDialog
          apiConfig={config}
          onRegistered={() => {}}
        />
      )

      await openDialog()
      await fillAndSubmit('acme-corp', 'Acme Corp')

      await waitFor(() => {
        expect(mockToastError).toHaveBeenCalledWith(
          expect.stringMatching(/validation error: name is required/i)
        )
      })
    })
  })
})
