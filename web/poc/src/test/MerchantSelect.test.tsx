import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ProvisionSpaceDialog } from '@/components/ProvisionSpaceDialog'
import type { ApiConfig, Merchant } from '@/lib/api'

const config: ApiConfig = {
  baseUrl: 'http://localhost:8080',
  apiKey: 'test-key-abc123',
}

const sampleMerchants: Merchant[] = [
  {
    id: 'aaaaaaaa-0000-0000-0000-000000000001',
    external_ref: 'acme-corp',
    name: 'Acme Corp',
    help_desk_url: null,
    invite_link: null,
    invite_link_set_at: null,
    is_active: true,
    created_at: '2026-06-09T00:00:00Z',
  },
  {
    id: 'aaaaaaaa-0000-0000-0000-000000000002',
    external_ref: 'globex',
    name: 'Globex',
    help_desk_url: null,
    invite_link: null,
    invite_link_set_at: null,
    is_active: true,
    created_at: '2026-06-09T00:00:00Z',
  },
]

beforeEach(() => {
  // Stub fetch for the listMerchants call triggered on dialog open.
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
    ok: true,
    status: 200,
    json: async () => ({ items: [], next_cursor: null }),
  }))
})

describe('ProvisionSpaceDialog — merchant Select', () => {
  it('renders a merchant select combobox when merchants are supplied', async () => {
    render(
      <ProvisionSpaceDialog
        apiConfig={config}
        onProvisioned={() => {}}
        merchants={sampleMerchants}
      />
    )

    // Open the dialog via the trigger button.
    await userEvent.click(screen.getByRole('button', { name: /provision space/i }))

    await waitFor(() => {
      expect(screen.getByRole('dialog')).toBeInTheDocument()
    })

    // The Select renders a combobox role — verifies merchant list path is active.
    expect(screen.getByRole('combobox')).toBeInTheDocument()

    // The manual UUID input should NOT appear when merchants are present.
    expect(screen.queryByLabelText(/merchant id \(uuid\)/i)).not.toBeInTheDocument()
  })

  it('shows manual UUID input when no merchants are available', async () => {
    render(
      <ProvisionSpaceDialog
        apiConfig={config}
        onProvisioned={() => {}}
        merchants={[]}
      />
    )

    await userEvent.click(screen.getByRole('button', { name: /provision space/i }))

    await waitFor(() => {
      expect(screen.getByRole('dialog')).toBeInTheDocument()
    })

    // No Select combobox — falls back to manual UUID input.
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument()
    expect(screen.getByLabelText(/merchant id \(uuid\)/i)).toBeInTheDocument()
  })
})
