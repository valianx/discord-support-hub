import { useState, useEffect, useCallback } from 'react'
import { toast } from 'sonner'
import { CheckCircle, XCircle, Loader2, Clock } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { Separator } from '@/components/ui/separator'
import { SettingsDialog } from '@/components/SettingsDialog'
import { ProvisionSpaceDialog } from '@/components/ProvisionSpaceDialog'
import { SpacesTable } from '@/components/SpacesTable'
import { listSpaces, type Space, type ApiConfig, ApiError } from '@/lib/api'
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

  const refreshConfig = useCallback(() => {
    setApiConfig({ baseUrl: getBaseUrl(), apiKey: getApiKey() })
  }, [])

  // Stable callbacks — these accept the config as a parameter so they are
  // not recreated when apiConfig changes (avoids effect retrigger loops).
  //
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

  // Snapshot values used as effect dependencies — only re-run when key or URL
  // actually change, without holding a stale closure over the full config object.
  const apiKey = apiConfig.apiKey
  const baseUrl = apiConfig.baseUrl

  useEffect(() => {
    const config: ApiConfig = { apiKey, baseUrl }
    void runCheckConnection(config) // eslint-disable-line react-hooks/set-state-in-effect
    void runLoadSpaces(config)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [apiKey, baseUrl])

  function handleSettingsChange() {
    refreshConfig()
  }

  function handleRefreshSpaces() {
    void runLoadSpaces(apiConfig)
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
