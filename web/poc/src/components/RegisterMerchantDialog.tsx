import { useState } from 'react'
import { Building2, Copy, Check } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { type ApiConfig, registerMerchant, ApiError, type Merchant } from '@/lib/api'
import { toast } from 'sonner'

interface RegisterMerchantDialogProps {
  apiConfig: ApiConfig
  onRegistered: (merchant: Merchant) => void
}

export function RegisterMerchantDialog({ apiConfig, onRegistered }: RegisterMerchantDialogProps) {
  const [open, setOpen] = useState(false)
  const [externalRef, setExternalRef] = useState('')
  const [name, setName] = useState('')
  const [helpDeskUrl, setHelpDeskUrl] = useState('')
  const [loading, setLoading] = useState(false)
  const [createdMerchant, setCreatedMerchant] = useState<Merchant | null>(null)
  const [idCopied, setIdCopied] = useState(false)

  function resetForm() {
    setExternalRef('')
    setName('')
    setHelpDeskUrl('')
    setCreatedMerchant(null)
    setIdCopied(false)
  }

  function handleOpenChange(isOpen: boolean) {
    if (!isOpen && !loading) {
      resetForm()
    }
    setOpen(isOpen)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!externalRef.trim() || !name.trim()) return

    setLoading(true)
    try {
      const merchant = await registerMerchant(apiConfig, {
        external_ref: externalRef.trim(),
        name: name.trim(),
        help_desk_url: helpDeskUrl.trim() || null,
      })

      setCreatedMerchant(merchant)
      toast.success(`Merchant "${merchant.name}" registered`)
      onRegistered(merchant)
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        toast.error('external_ref already exists — this merchant is already registered')
      } else if (err instanceof ApiError && err.status === 400) {
        toast.error(`Validation error: ${err.message}`)
      } else {
        const message = err instanceof Error ? err.message : String(err)
        toast.error(`Registration failed: ${message}`)
      }
    } finally {
      setLoading(false)
    }
  }

  async function handleCopyId() {
    if (!createdMerchant) return
    await navigator.clipboard.writeText(createdMerchant.id)
    setIdCopied(true)
    setTimeout(() => setIdCopied(false), 2000)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          <Building2 className="h-4 w-4 mr-2" />
          Register Merchant
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Register a Merchant</DialogTitle>
          <DialogDescription>
            Register a new merchant in the hub. The merchant UUID can then be used to provision a support space.
          </DialogDescription>
        </DialogHeader>

        {!createdMerchant ? (
          <form onSubmit={handleSubmit}>
            <div className="grid gap-4 py-2">
              <div className="grid gap-2">
                <Label htmlFor="merchant-external-ref">External Ref (required)</Label>
                <Input
                  id="merchant-external-ref"
                  placeholder="e.g. acme-corp"
                  value={externalRef}
                  onChange={(e) => setExternalRef(e.target.value)}
                  disabled={loading}
                  required
                />
                <p className="text-xs text-slate-500">
                  Your stable backoffice identifier — must be unique.
                </p>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="merchant-name">Name (required)</Label>
                <Input
                  id="merchant-name"
                  placeholder="e.g. Acme Corp"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  disabled={loading}
                  required
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="merchant-help-desk-url">Help Desk URL (optional)</Label>
                <Input
                  id="merchant-help-desk-url"
                  type="url"
                  placeholder="https://help.example.com"
                  value={helpDeskUrl}
                  onChange={(e) => setHelpDeskUrl(e.target.value)}
                  disabled={loading}
                />
              </div>
            </div>

            <DialogFooter className="mt-4">
              <Button type="submit" disabled={loading}>
                {loading ? 'Registering…' : 'Register'}
              </Button>
            </DialogFooter>
          </form>
        ) : (
          <div className="grid gap-4 py-2">
            <p className="text-sm text-slate-700">
              Merchant <strong>{createdMerchant.name}</strong> registered. Copy the UUID below — you will need it to provision a support space.
            </p>
            <div className="grid gap-2">
              <Label>Merchant ID</Label>
              <div className="flex items-center gap-2">
                <Input
                  readOnly
                  value={createdMerchant.id}
                  className="font-mono text-xs"
                />
                <Button
                  variant="outline"
                  size="icon"
                  onClick={handleCopyId}
                  title="Copy merchant ID"
                >
                  {idCopied ? (
                    <Check className="h-4 w-4 text-green-600" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              </div>
            </div>
            <div className="text-xs text-slate-500 space-y-0.5">
              <p><span className="font-medium">External ref:</span> {createdMerchant.external_ref}</p>
              {createdMerchant.help_desk_url && (
                <p><span className="font-medium">Help desk:</span> {createdMerchant.help_desk_url}</p>
              )}
            </div>
            <DialogFooter>
              <Button onClick={() => handleOpenChange(false)}>Done</Button>
            </DialogFooter>
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
