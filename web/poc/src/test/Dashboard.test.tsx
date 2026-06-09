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
  sessionStorage.clear()
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

  it('renders the Settings button', () => {
    render(<App />)
    expect(screen.getByRole('button', { name: /settings/i })).toBeInTheDocument()
  })

  it('renders the security banner', () => {
    render(<App />)
    expect(
      screen.getByText(/local operator tool/i)
    ).toBeInTheDocument()
  })

  it('prompts for API key when none is set', () => {
    render(<App />)
    expect(screen.getByText(/no api key configured/i)).toBeInTheDocument()
  })
})
