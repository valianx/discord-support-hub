import { useState } from 'react'
import { Link } from 'lucide-react'
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
} from '@/components/ui/dialog'
import { type ApiConfig, type Merchant, setMerchantInviteLink, ApiError } from '@/lib/api'
import { toast } from 'sonner'

interface MerchantInviteLinkDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  merchant: Merchant
  apiConfig: ApiConfig
  onUpdated: (merchant: Merchant) => void
}

export function MerchantInviteLinkDialog({
  open,
  onOpenChange,
  merchant,
  apiConfig,
  onUpdated,
}: MerchantInviteLinkDialogProps) {
  const [inviteLink, setInviteLinkValue] = useState(merchant.invite_link ?? '')
  const [saving, setSaving] = useState(false)

  function handleOpenChange(isOpen: boolean) {
    if (isOpen) {
      setInviteLinkValue(merchant.invite_link ?? '')
    }
    onOpenChange(isOpen)
  }

  async function handleSave(e: React.FormEvent) {
    e.preventDefault()
    if (!inviteLink.trim()) {
      toast.error('Invite link is required')
      return
    }

    setSaving(true)
    try {
      const updated = await setMerchantInviteLink(apiConfig, merchant.id, inviteLink.trim())
      onUpdated(updated)
      toast.success(`Invite link saved for ${merchant.name}`)
      onOpenChange(false)
    } catch (err) {
      if (err instanceof ApiError && err.status === 400) {
        toast.error(`Validation error: ${err.message}`)
      } else if (err instanceof ApiError && err.status === 404) {
        toast.error('Merchant not found')
      } else {
        const message = err instanceof Error ? err.message : String(err)
        toast.error(`Failed to save invite link: ${message}`)
      }
    } finally {
      setSaving(false)
    }
  }

  const isSet = Boolean(merchant.invite_link)

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Merchant Invite Link — {merchant.name}</DialogTitle>
          <DialogDescription>
            The native Discord invite-with-role link for this merchant. Collaborators receive this
            link by email when you send an invitation.
          </DialogDescription>
        </DialogHeader>

        <div className="rounded-md bg-amber-50 border border-amber-200 px-3 py-2 text-sm text-amber-800">
          Create this link in the Discord client via <strong>Server Settings → Invites</strong>{' '}
          and select the merchant's role under "Roles (optional)". The link is reusable — one link
          covers all collaborators for this merchant.
        </div>

        <form onSubmit={handleSave}>
          <div className="grid gap-4 py-2">
            <div className="grid gap-2">
              <Label htmlFor="invite-link">
                Invite Link{' '}
                <span className="text-slate-400 font-normal text-xs">
                  (https://discord.gg/... or https://discord.com/invite/...)
                </span>
              </Label>
              <div className="flex items-center gap-2">
                <Link className="h-4 w-4 text-slate-400 shrink-0" />
                <Input
                  id="invite-link"
                  type="url"
                  placeholder="https://discord.gg/XXXXXXXXX"
                  value={inviteLink}
                  onChange={(e) => setInviteLinkValue(e.target.value)}
                  disabled={saving}
                  className="font-mono text-sm"
                />
              </div>
              {isSet && merchant.invite_link_set_at && (
                <p className="text-xs text-slate-500">
                  Last set:{' '}
                  {new Intl.DateTimeFormat('en', {
                    dateStyle: 'medium',
                    timeStyle: 'short',
                  }).format(new Date(merchant.invite_link_set_at))}
                </p>
              )}
              {!isSet && (
                <p className="text-xs text-amber-700">
                  No invite link set. Collaborator invitations will fail until this is saved.
                </p>
              )}
            </div>
          </div>
          <DialogFooter className="mt-4">
            <Button type="submit" disabled={saving}>
              {saving ? 'Saving…' : 'Save Link'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
