import { useState } from 'react'
import { Copy, Check } from 'lucide-react'
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
import { type ApiConfig, inviteCollaborator } from '@/lib/api'
import { toast } from 'sonner'

interface InviteCollaboratorDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  spaceId: string
  spaceName: string
  apiConfig: ApiConfig
}

export function InviteCollaboratorDialog({
  open,
  onOpenChange,
  spaceId,
  spaceName,
  apiConfig,
}: InviteCollaboratorDialogProps) {
  const [email, setEmail] = useState('')
  const [discordId, setDiscordId] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [loading, setLoading] = useState(false)
  const [connectUrl, setConnectUrl] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  function handleOpenChange(isOpen: boolean) {
    if (!isOpen) {
      setEmail('')
      setDiscordId('')
      setDisplayName('')
      setConnectUrl(null)
      setCopied(false)
    }
    onOpenChange(isOpen)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!email.trim() && !discordId.trim()) {
      toast.error('Provide an email or Discord ID')
      return
    }

    setLoading(true)
    try {
      const result = await inviteCollaborator(apiConfig, spaceId, {
        email: email.trim() || null,
        discord_user_id: discordId.trim() || null,
        display_name: displayName.trim() || null,
      })

      toast.success('Invite accepted (202)')

      if (result.connect_url) {
        setConnectUrl(result.connect_url)
      } else {
        handleOpenChange(false)
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err)
      toast.error(`Invite failed: ${message}`)
    } finally {
      setLoading(false)
    }
  }

  async function handleCopyUrl() {
    if (!connectUrl) return
    await navigator.clipboard.writeText(connectUrl)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Invite Collaborator</DialogTitle>
          <DialogDescription>
            Invite a collaborator to <strong>{spaceName}</strong>.
          </DialogDescription>
        </DialogHeader>

        {!connectUrl ? (
          <form onSubmit={handleSubmit}>
            <div className="grid gap-4 py-2">
              <div className="grid gap-2">
                <Label htmlFor="collab-email">Email</Label>
                <Input
                  id="collab-email"
                  type="email"
                  placeholder="user@example.com"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  disabled={loading}
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="collab-discord-id">Discord User ID</Label>
                <Input
                  id="collab-discord-id"
                  placeholder="Snowflake ID (optional if email given)"
                  value={discordId}
                  onChange={(e) => setDiscordId(e.target.value)}
                  disabled={loading}
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="collab-display-name">Display Name (optional)</Label>
                <Input
                  id="collab-display-name"
                  placeholder="Jane Doe"
                  value={displayName}
                  onChange={(e) => setDisplayName(e.target.value)}
                  disabled={loading}
                />
              </div>
            </div>
            <DialogFooter className="mt-4">
              <Button type="submit" disabled={loading}>
                {loading ? 'Sending…' : 'Invite'}
              </Button>
            </DialogFooter>
          </form>
        ) : (
          <div className="grid gap-4 py-2">
            <p className="text-sm text-slate-700">
              The collaborator must complete{' '}
              <strong>Connect with Discord</strong> before they can access the
              space. Share this one-time OAuth2 link:
            </p>
            <div className="flex items-center gap-2">
              <Input
                readOnly
                value={connectUrl}
                className="font-mono text-xs"
              />
              <Button
                variant="outline"
                size="icon"
                onClick={handleCopyUrl}
                title="Copy to clipboard"
              >
                {copied ? (
                  <Check className="h-4 w-4 text-green-600" />
                ) : (
                  <Copy className="h-4 w-4" />
                )}
              </Button>
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
