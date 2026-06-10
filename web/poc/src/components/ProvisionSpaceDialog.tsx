import { useState, useRef } from 'react'
import { Plus } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  type ApiConfig,
  provisionSpace,
  listMerchants,
  pollJob,
  type Job,
  type JobStatus,
  type Merchant,
  ApiError,
} from '@/lib/api'
import { toast } from 'sonner'

interface ProvisionSpaceDialogProps {
  apiConfig: ApiConfig
  onProvisioned: () => void
  /** Merchant list from the parent — kept fresh after Register Merchant. */
  merchants?: Merchant[]
}

function jobStatusVariant(status: JobStatus) {
  switch (status) {
    case 'completed': return 'success' as const
    case 'archived': return 'destructive' as const
    case 'active':
    case 'retrying': return 'warning' as const
    default: return 'secondary' as const
  }
}

export function ProvisionSpaceDialog({ apiConfig, onProvisioned, merchants: propMerchants }: ProvisionSpaceDialogProps) {
  const [open, setOpen] = useState(false)
  const [merchantId, setMerchantId] = useState('')
  const [manualMerchantId, setManualMerchantId] = useState('')
  const [spaceName, setSpaceName] = useState('')
  const [loading, setLoading] = useState(false)
  const [activeJob, setActiveJob] = useState<Job | null>(null)
  // Locally-fetched fallback when the parent provides no merchants yet.
  const [fetchedMerchants, setFetchedMerchants] = useState<Merchant[]>([])
  const [merchantsLoading, setMerchantsLoading] = useState(false)
  // Avoid re-fetching on every open if the parent already supplies merchants.
  const hasFetched = useRef(false)

  // Prefer the parent's list (always current after registrations); fall back to fetched.
  const merchants = (propMerchants && propMerchants.length > 0)
    ? propMerchants
    : fetchedMerchants

  async function fetchMerchants() {
    if (!apiConfig.apiKey || hasFetched.current) return
    setMerchantsLoading(true)
    try {
      const result = await listMerchants(apiConfig, { is_active: true })
      setFetchedMerchants(result.items)
      hasFetched.current = true
    } catch (err) {
      const message =
        err instanceof ApiError
          ? `[${err.code}] ${err.message}`
          : 'Could not load merchants — check your API key and hub URL.'
      toast.error('Failed to load merchants', { description: message })
      // Operator can still fall back to the manual UUID field.
    } finally {
      setMerchantsLoading(false)
    }
  }

  function handleOpenChange(isOpen: boolean) {
    if (isOpen) {
      void fetchMerchants()
    }
    if (!isOpen && !loading) {
      setMerchantId('')
      setManualMerchantId('')
      setSpaceName('')
      setActiveJob(null)
    }
    setOpen(isOpen)
  }

  function resolvedMerchantId(): string {
    if (merchantId && merchantId !== '__manual__') return merchantId
    return manualMerchantId.trim()
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    const mid = resolvedMerchantId()
    if (!mid || !spaceName.trim()) return

    setLoading(true)
    setActiveJob(null)

    try {
      const result = await provisionSpace(apiConfig, mid, {
        name: spaceName.trim(),
      })
      const job = result.job
      setActiveJob(job)
      toast.info(`Job accepted: ${job.id}`)

      const finalJob = await pollJob(
        apiConfig,
        job.id,
        (updated) => setActiveJob(updated)
      )

      if (finalJob.status === 'completed') {
        toast.success('Space provisioned successfully')
        onProvisioned()
        setOpen(false)
      } else {
        toast.error(`Provisioning failed: ${finalJob.error ?? 'unknown error'}`)
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err)
      toast.error(`Error: ${message}`)
    } finally {
      setLoading(false)
    }
  }

  const hasMerchants = merchants.length > 0
  const showManualInput = !hasMerchants || merchantId === '__manual__'

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger asChild>
        <Button size="sm">
          <Plus className="h-4 w-4 mr-2" />
          Provision Space
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Provision a Space</DialogTitle>
          <DialogDescription>
            Create a private Discord support space for a merchant.
          </DialogDescription>
        </DialogHeader>

        <form onSubmit={handleSubmit}>
          <div className="grid gap-4 py-2">
            <div className="grid gap-2">
              <Label htmlFor="provision-merchant">Merchant</Label>
              {hasMerchants ? (
                <Select
                  value={merchantId}
                  onValueChange={setMerchantId}
                  disabled={loading || merchantsLoading}
                >
                  <SelectTrigger id="provision-merchant">
                    <SelectValue placeholder={merchantsLoading ? 'Loading merchants…' : 'Select a merchant'} />
                  </SelectTrigger>
                  <SelectContent>
                    {merchants.map((m) => (
                      <SelectItem key={m.id} value={m.id}>
                        {m.name} ({m.external_ref})
                      </SelectItem>
                    ))}
                    <SelectItem value="__manual__">Enter UUID manually…</SelectItem>
                  </SelectContent>
                </Select>
              ) : (
                <p className="text-xs text-slate-500">
                  No merchants found — enter a UUID below, or register a merchant first.
                </p>
              )}
            </div>

            {showManualInput && (
              <div className="grid gap-2">
                <Label htmlFor="merchant-id-manual">Merchant ID (UUID)</Label>
                <Input
                  id="merchant-id-manual"
                  placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
                  value={manualMerchantId}
                  onChange={(e) => setManualMerchantId(e.target.value)}
                  disabled={loading}
                  required={showManualInput}
                />
              </div>
            )}

            <div className="grid gap-2">
              <Label htmlFor="space-name">Space Name</Label>
              <Input
                id="space-name"
                placeholder="e.g. acme-support"
                value={spaceName}
                onChange={(e) => setSpaceName(e.target.value)}
                disabled={loading}
                required
              />
            </div>

            {activeJob && (
              <div className="rounded-md border border-slate-200 p-3 bg-slate-50">
                <div className="flex items-center gap-2 text-sm">
                  <span className="text-slate-600">Job:</span>
                  <code className="text-xs bg-slate-100 px-1 rounded">
                    {activeJob.id.slice(0, 8)}…
                  </code>
                  <Badge variant={jobStatusVariant(activeJob.status)}>
                    {activeJob.status}
                  </Badge>
                </div>
                {activeJob.error && (
                  <p className="text-xs text-red-600 mt-1">{activeJob.error}</p>
                )}
              </div>
            )}
          </div>

          <DialogFooter className="mt-4">
            <Button type="submit" disabled={loading || !resolvedMerchantId()}>
              {loading ? 'Provisioning…' : 'Provision'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
