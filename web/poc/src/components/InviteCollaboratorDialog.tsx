import { useState } from 'react'
import { Send } from 'lucide-react'
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
import {
  type ApiConfig,
  type SpaceMember,
  registerCollaborator,
  sendCollaboratorInvite,
  ApiError,
} from '@/lib/api'
import { toast } from 'sonner'

interface InviteCollaboratorDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  spaceId: string
  spaceName: string
  apiConfig: ApiConfig
  /** Pass false when the space's merchant has no invite link set, to proactively disable Send Invitation. */
  merchantInviteLinkSet?: boolean
}

type DialogStep = 'register' | 'registered'

export function InviteCollaboratorDialog({
  open,
  onOpenChange,
  spaceId,
  spaceName,
  apiConfig,
  merchantInviteLinkSet = true,
}: InviteCollaboratorDialogProps) {
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [registering, setRegistering] = useState(false)
  const [sendingInvite, setSendingInvite] = useState(false)
  const [step, setStep] = useState<DialogStep>('register')
  const [registeredMember, setRegisteredMember] = useState<SpaceMember | null>(null)

  function resetForm() {
    setName('')
    setEmail('')
    setStep('register')
    setRegisteredMember(null)
  }

  function handleOpenChange(isOpen: boolean) {
    if (!isOpen) {
      resetForm()
    }
    onOpenChange(isOpen)
  }

  async function handleRegister(e: React.FormEvent) {
    e.preventDefault()
    if (!name.trim() || !email.trim()) {
      toast.error('Name and email are required')
      return
    }

    setRegistering(true)
    try {
      const member = await registerCollaborator(apiConfig, spaceId, {
        name: name.trim(),
        email: email.trim(),
      })
      setRegisteredMember(member)
      setStep('registered')
      toast.success(`${name.trim()} registered on this space`)
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        toast.error('This collaborator is already registered on this space')
      } else {
        const message = err instanceof Error ? err.message : String(err)
        toast.error(`Registration failed: ${message}`)
      }
    } finally {
      setRegistering(false)
    }
  }

  async function handleSendInvite() {
    if (!registeredMember) return

    setSendingInvite(true)
    try {
      await sendCollaboratorInvite(apiConfig, spaceId, registeredMember.user_id)
      toast.success(`Invitation email queued for ${registeredMember.display_name ?? email}`)
      handleOpenChange(false)
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        toast.error(
          'No invite link set for this merchant. Set one via Merchant → Set Invite Link first.',
          { duration: 6000 }
        )
      } else {
        const message = err instanceof Error ? err.message : String(err)
        toast.error(`Failed to send invite: ${message}`)
      }
    } finally {
      setSendingInvite(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Register Collaborator</DialogTitle>
          <DialogDescription>
            Add a collaborator to <strong>{spaceName}</strong> by name and work email.
          </DialogDescription>
        </DialogHeader>

        {step === 'register' && (
          <form onSubmit={handleRegister}>
            <div className="grid gap-4 py-2">
              <div className="grid gap-2">
                <Label htmlFor="collab-name">Full Name</Label>
                <Input
                  id="collab-name"
                  placeholder="Jane Doe"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  disabled={registering}
                  required
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="collab-email">Work Email</Label>
                <Input
                  id="collab-email"
                  type="email"
                  placeholder="jane@example.com"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  disabled={registering}
                  required
                />
              </div>
            </div>
            <DialogFooter className="mt-4">
              <Button type="submit" disabled={registering}>
                {registering ? 'Registering…' : 'Register'}
              </Button>
            </DialogFooter>
          </form>
        )}

        {step === 'registered' && registeredMember && (
          <div className="grid gap-4 py-2">
            <p className="text-sm text-slate-700">
              <strong>{registeredMember.display_name ?? email}</strong> is now registered on this
              space. Send the merchant's invite-with-role link to their email to grant Discord
              access.
            </p>
            {merchantInviteLinkSet ? (
              <div className="rounded-md bg-blue-50 border border-blue-200 px-3 py-2 text-sm text-blue-800">
                The invite email will be sent from the hub's SMTP relay using the merchant's stored
                invite link.
              </div>
            ) : (
              <div className="rounded-md bg-amber-50 border border-amber-200 px-3 py-2 text-sm text-amber-800">
                This merchant has no invite link set — sending will fail. Set it first via{' '}
                <strong>Merchant → Set Invite Link</strong>.
              </div>
            )}
            <DialogFooter className="flex-row justify-between sm:justify-between mt-2">
              <Button variant="outline" onClick={() => handleOpenChange(false)}>
                Done (send later)
              </Button>
              <Button
                onClick={handleSendInvite}
                disabled={sendingInvite || !merchantInviteLinkSet}
                title={
                  !merchantInviteLinkSet
                    ? 'Set this merchant\'s invite link first'
                    : undefined
                }
              >
                <Send className="h-4 w-4 mr-2" />
                {sendingInvite ? 'Sending…' : 'Send Invitation'}
              </Button>
            </DialogFooter>
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
