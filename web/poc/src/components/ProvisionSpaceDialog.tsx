import { useState } from 'react'
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
import { type ApiConfig, provisionSpace, pollJob, type Job, type JobStatus } from '@/lib/api'
import { toast } from 'sonner'

interface ProvisionSpaceDialogProps {
  apiConfig: ApiConfig
  onProvisioned: () => void
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

export function ProvisionSpaceDialog({ apiConfig, onProvisioned }: ProvisionSpaceDialogProps) {
  const [open, setOpen] = useState(false)
  const [merchantId, setMerchantId] = useState('')
  const [spaceName, setSpaceName] = useState('')
  const [loading, setLoading] = useState(false)
  const [activeJob, setActiveJob] = useState<Job | null>(null)

  function handleOpenChange(isOpen: boolean) {
    if (!isOpen && !loading) {
      setMerchantId('')
      setSpaceName('')
      setActiveJob(null)
    }
    setOpen(isOpen)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!merchantId.trim() || !spaceName.trim()) return

    setLoading(true)
    setActiveJob(null)

    try {
      const result = await provisionSpace(apiConfig, merchantId.trim(), {
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
              <Label htmlFor="merchant-id">Merchant ID (UUID)</Label>
              <Input
                id="merchant-id"
                placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
                value={merchantId}
                onChange={(e) => setMerchantId(e.target.value)}
                disabled={loading}
                required
              />
            </div>
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
            <Button type="submit" disabled={loading}>
              {loading ? 'Provisioning…' : 'Provision'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
