import { useState, useEffect, useCallback } from 'react'
import { toast } from 'sonner'
import { CheckCircle, XCircle, Loader2, Clock, Building2 } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { Separator } from '@/components/ui/separator'
import { SettingsDialog } from '@/components/SettingsDialog'
import { ProvisionSpaceDialog } from '@/components/ProvisionSpaceDialog'
import { RegisterMerchantDialog } from '@/components/RegisterMerchantDialog'
import { SpacesTable } from '@/components/SpacesTable'
import { listSpaces, listMerchants, type Space, type Merchant, type ApiConfig, ApiError } from '@/lib/api'
import { getApiKey, getBaseUrl, hasApiKey } from '@/lib/settings'

type ConnectionStatus = 'unknown' | 'checking' | 'ok' | 'error'

export function Dashboard() {
  const [apiConfig, setApiConfig] = useState<ApiConfig>({
    baseUrl: getBaseUrl(),
    apiKey: getApiKey(),
  })
  const [spaces, setSpaces] = useState<Space[]>([])
  const [spacesLoading, setSpacesLoading] = useState(false)
  const [spacesLoadError, setSpacesLoadError] = useState(false)
  const [connectionStatus, setConnectionStatus] = useState<ConnectionStatus>('unknown')
  const [merchants, setMerchants] = useState<Merchant[]>([])
  const [merchantsLoadError, setMerchantsLoadError] = useState(false)

  const refreshConfig = useCallback(() => {
    setApiConfig({ baseUrl: getBaseUrl(), apiKey: getApiKey() })
  }, [])

  // Uses an authenticated probe (GET /v1/channels?limit=1) instead of the
  // unauthenticated /readyz so "Connected" means the key actually works.
  const runCheckConnection = useCallback(async (config: ApiConfig) => {
    if (!config.apiKey) {
      setConnectionStatus('unknown')
      return
    }
    setConnectionStatus('checking')
    try {
      await listSpaces(config, { limit: 1 })
      setConnectionStatus('ok')
    } catch {
      setConnectionStatus('error')
    }
  }, [])

  const runLoadSpaces = useCallback(async (config: ApiConfig) => {
    if (!config.apiKey) return
    setSpacesLoading(true)
    setSpacesLoadError(false)
    try {
      const result = await listSpaces(config)
      setSpaces(result.items)
    } catch (err) {
      setSpaces([])
      setSpacesLoadError(true)
      const message =
        err instanceof ApiError
          ? `[${err.code}] ${err.message}`
          : 'Could not load spaces — check your API key and hub URL.'
      toast.error('Failed to load spaces', { description: message })
    } finally {
      setSpacesLoading(false)
    }
  }, [])

  const runLoadMerchants = useCallback(async (config: ApiConfig) => {
    if (!config.apiKey) return
    setMerchantsLoadError(false)
    try {
      const result = await listMerchants(config, { is_active: true })
      setMerchants(result.items)
    } catch (err) {
      setMerchantsLoadError(true)
      const message =
        err instanceof ApiError
          ? `[${err.code}] ${err.message}`
          : 'Could not load merchants — check your API key and hub URL.'
      toast.error('Failed to load merchants', { description: message })
    }
  }, [])

  // Snapshot values used as effect dependencies — only re-run when key or URL
  // actually change, without holding a stale closure over the full config object.
  const apiKey = apiConfig.apiKey
  const baseUrl = apiConfig.baseUrl

  useEffect(() => {
    const config: ApiConfig = { apiKey, baseUrl }
    void runCheckConnection(config) // eslint-disable-line react-hooks/set-state-in-effect
    void runLoadSpaces(config)
    void runLoadMerchants(config)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [apiKey, baseUrl])

  function handleSettingsChange() {
    refreshConfig()
  }

  function handleRefreshSpaces() {
    void runLoadSpaces(apiConfig)
  }

  function handleMerchantRegistered(merchant: Merchant) {
    setMerchants((prev) => {
      // Avoid duplicates if the list already contains this merchant.
      const exists = prev.some((m) => m.id === merchant.id)
      return exists ? prev : [...prev, merchant]
    })
  }

  return (
    <div className="min-h-screen bg-slate-50">
      {/* Header */}
      <header className="bg-white border-b border-slate-200 px-6 py-4">
        <div className="max-w-7xl mx-auto flex items-center justify-between">
          <div className="flex items-center gap-3">
            <h1 className="text-xl font-bold text-slate-900">
              Discord Support Hub
            </h1>
            <Badge variant="secondary" className="text-xs">
              Backoffice POC
            </Badge>
          </div>

          <div className="flex items-center gap-4">
            <ConnectionStatusIndicator status={connectionStatus} />
            <SettingsDialog onSettingsChange={handleSettingsChange} />
          </div>
        </div>
      </header>

      <main className="max-w-7xl mx-auto px-6 py-6 grid gap-6">
        {/* Security banner */}
        <div className="rounded-md bg-amber-50 border border-amber-200 px-4 py-2 text-sm text-amber-800">
          Local operator tool — you supply your own backoffice service key; it
          stays in this browser session and is never sent anywhere except the
          hub. Do not deploy this publicly.
        </div>

        {!hasApiKey() && (
          <Card>
            <CardContent className="py-6 text-center">
              <p className="text-slate-500 text-sm">
                No API key configured. Click <strong>Settings</strong> to enter
                your backoffice service API key.
              </p>
            </CardContent>
          </Card>
        )}

        {hasApiKey() && (
          <>
            {/* Merchants section */}
            <section>
              <div className="flex items-center justify-between mb-3">
                <div>
                  <h2 className="text-lg font-semibold">Merchants</h2>
                  <p className="text-sm text-slate-500">
                    {merchantsLoadError
                      ? 'Merchant list could not be loaded — see the error notification.'
                      : merchants.length === 0
                        ? 'No active merchants yet — register one to get started.'
                        : `${merchants.length} active merchant${merchants.length === 1 ? '' : 's'} registered.`}
                  </p>
                </div>
                <RegisterMerchantDialog
                  apiConfig={apiConfig}
                  onRegistered={handleMerchantRegistered}
                />
              </div>

              {merchants.length > 0 && (
                <MerchantList merchants={merchants} />
              )}
              <Separator className="mt-3" />
            </section>

            {/* Provision section */}
            <section>
              <div className="flex items-center justify-between mb-3">
                <div>
                  <h2 className="text-lg font-semibold">Provision</h2>
                  <p className="text-sm text-slate-500">
                    Create a new private support space for a merchant.
                  </p>
                </div>
                <ProvisionSpaceDialog
                  apiConfig={apiConfig}
                  onProvisioned={handleRefreshSpaces}
                  merchants={merchants}
                />
              </div>
              <Separator />
            </section>

            {/* Spaces table */}
            <section>
              <SpacesTable
                spaces={spaces}
                loading={spacesLoading}
                loadError={spacesLoadError}
                apiConfig={apiConfig}
                onRefresh={handleRefreshSpaces}
                onChanged={handleRefreshSpaces}
              />
            </section>
          </>
        )}
      </main>
    </div>
  )
}

function MerchantList({ merchants }: { merchants: Merchant[] }) {
  return (
    <div className="flex flex-wrap gap-2 py-2">
      {merchants.map((m) => (
        <div
          key={m.id}
          className="flex items-center gap-1.5 rounded-md border border-slate-200 bg-white px-3 py-1.5 text-sm"
        >
          <Building2 className="h-3.5 w-3.5 text-slate-400 shrink-0" />
          <span className="font-medium text-slate-800">{m.name}</span>
          <span className="text-slate-400 text-xs">({m.external_ref})</span>
        </div>
      ))}
    </div>
  )
}

function ConnectionStatusIndicator({ status }: { status: ConnectionStatus }) {
  switch (status) {
    case 'checking':
      return (
        <div className="flex items-center gap-1.5 text-slate-400 text-sm">
          <Loader2 className="h-4 w-4 animate-spin" />
          Connecting…
        </div>
      )
    case 'ok':
      return (
        <div className="flex items-center gap-1.5 text-green-600 text-sm">
          <CheckCircle className="h-4 w-4" />
          Connected
        </div>
      )
    case 'error':
      return (
        <div className="flex items-center gap-1.5 text-red-500 text-sm">
          <XCircle className="h-4 w-4" />
          Unreachable
        </div>
      )
    default:
      return (
        <div className="flex items-center gap-1.5 text-slate-400 text-sm">
          <Clock className="h-4 w-4" />
          Not checked
        </div>
      )
  }
}
