import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import App from '@/App'

// Stub fetch to prevent actual network calls during smoke render.
beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
    ok: false,
    status: 503,
    json: async () => ({ code: 'unavailable', message: 'Service unavailable' }),
  }))
})

describe('Dashboard smoke render', () => {
  it('renders the app title', () => {
    render(<App />)
    expect(screen.getByText('Discord Support Hub')).toBeInTheDocument()
  })

  it('renders the POC badge', () => {
    render(<App />)
    expect(screen.getByText('Backoffice POC')).toBeInTheDocument()
  })

  it('renders the Merchants section heading', () => {
    render(<App />)
    expect(screen.getByText('Merchants')).toBeInTheDocument()
  })

  it('renders the Provision section heading', () => {
    render(<App />)
    expect(screen.getByText('Provision')).toBeInTheDocument()
  })
})
