import { useState, useEffect, useCallback } from 'react'
import { Send } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { ScrollArea } from '@/components/ui/scroll-area'
import {
  type ApiConfig,
  type SpaceMember,
  listMembers,
  expelCollaborator,
  sendCollaboratorInvite,
  pollJob,
  ApiError,
} from '@/lib/api'
import { toast } from 'sonner'

interface MembersDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  spaceId: string
  spaceName: string
  apiConfig: ApiConfig
  /** Pass false when the space's merchant has no invite link set, to disable the Invite button proactively. */
  merchantInviteLinkSet?: boolean
}

function formatDate(iso: string | null | undefined): string {
  if (!iso) return 'Not sent'
  return new Intl.DateTimeFormat('en', {
    dateStyle: 'short',
    timeStyle: 'short',
  }).format(new Date(iso))
}

export function MembersDialog({
  open,
  onOpenChange,
  spaceId,
  spaceName,
  apiConfig,
  merchantInviteLinkSet = true,
}: MembersDialogProps) {
  const [members, setMembers] = useState<SpaceMember[]>([])
  const [loading, setLoading] = useState(false)
  const [expellingId, setExpellingId] = useState<string | null>(null)
  const [sendingInviteId, setSendingInviteId] = useState<string | null>(null)
  const [expelScopes, setExpelScopes] = useState<Record<string, 'channel' | 'server'>>({})
  // Member pending confirmation before the expel fires.
  const [pendingExpelMember, setPendingExpelMember] = useState<SpaceMember | null>(null)

  const loadMembers = useCallback(async () => {
    setLoading(true)
    try {
      const result = await listMembers(apiConfig, spaceId)
      setMembers(result.items)
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err)
      toast.error(`Failed to load members: ${message}`)
    } finally {
      setLoading(false)
    }
  }, [apiConfig, spaceId])

  useEffect(() => {
    if (!open) return
    // Data-fetching effect — intentionally calls setState inside async work.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void loadMembers()
  }, [open, loadMembers])

  function getScopeForMember(userId: string): 'channel' | 'server' {
    return expelScopes[userId] ?? 'channel'
  }

  function setScopeForMember(userId: string, scope: 'channel' | 'server') {
    setExpelScopes((prev) => ({ ...prev, [userId]: scope }))
  }

  /** Opens the confirmation dialog; actual expel fires on confirm. */
  function handleExpel(member: SpaceMember) {
    setPendingExpelMember(member)
  }

  /** Called only after the operator clicks Confirm in the AlertDialog. */
  async function doExpel(member: SpaceMember) {
    const scope = getScopeForMember(member.user_id)
    setExpellingId(member.user_id)
    setPendingExpelMember(null)

    try {
      const result = await expelCollaborator(apiConfig, spaceId, member.user_id, scope)
      toast.info(`Expulsion job accepted: ${result.job.id.slice(0, 8)}…`)

      const finalJob = await pollJob(apiConfig, result.job.id, () => {})

      if (finalJob.status === 'completed') {
        toast.success(`Collaborator expelled from ${scope}`)
        setMembers((prev) => prev.filter((m) => m.user_id !== member.user_id))
      } else {
        toast.error(`Expulsion failed: ${finalJob.error ?? 'unknown'}`)
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err)
      toast.error(`Expulsion error: ${message}`)
    } finally {
      setExpellingId(null)
    }
  }

  async function handleSendInvite(member: SpaceMember) {
    setSendingInviteId(member.user_id)
    try {
      await sendCollaboratorInvite(apiConfig, spaceId, member.user_id)
      toast.success(`Invitation queued for ${member.display_name ?? member.user_id.slice(0, 8)}`)
      // Refresh members so invite_sent_at updates.
      void loadMembers()
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        toast.error(
          'No invite link set for this merchant. Set it via Merchant → Set Invite Link.',
          { duration: 6000 }
        )
      } else {
        const message = err instanceof Error ? err.message : String(err)
        toast.error(`Failed to send invite: ${message}`)
      }
    } finally {
      setSendingInviteId(null)
    }
  }

  const pendingScope = pendingExpelMember
    ? getScopeForMember(pendingExpelMember.user_id)
    : 'channel'

  return (
    <>
    <AlertDialog
      open={pendingExpelMember !== null}
      onOpenChange={(isOpen) => { if (!isOpen) setPendingExpelMember(null) }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Confirm expulsion</AlertDialogTitle>
          <AlertDialogDescription>
            Remove{' '}
            <strong>
              {pendingExpelMember?.display_name ??
                pendingExpelMember?.email ??
                pendingExpelMember?.user_id.slice(0, 8) + '…'}
            </strong>{' '}
            from the <strong>{pendingScope}</strong>
            {pendingScope === 'server' ? ' (entire Discord guild)' : ''}? This action cannot be
            undone from this UI.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            className="bg-red-500 text-white hover:bg-red-600 focus-visible:ring-red-500"
            onClick={() => { if (pendingExpelMember) void doExpel(pendingExpelMember) }}
          >
            Expel
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>

    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Members — {spaceName}</DialogTitle>
          <DialogDescription>
            Collaborators registered on this space.
          </DialogDescription>
        </DialogHeader>

        {loading && (
          <p className="text-sm text-slate-500 py-4 text-center">Loading…</p>
        )}

        {!loading && members.length === 0 && (
          <p className="text-sm text-slate-500 py-4 text-center">
            No members found.
          </p>
        )}

        {!loading && members.length > 0 && (
          <ScrollArea className="max-h-96">
            <div className="grid gap-2">
              {members.map((member) => (
                <div
                  key={member.user_id}
                  className="rounded-md border border-slate-200 px-3 py-2"
                >
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex flex-col gap-0.5 min-w-0">
                      <span className="text-sm font-medium truncate">
                        {member.display_name ?? member.user_id.slice(0, 8) + '…'}
                      </span>
                      <div className="flex items-center gap-2 flex-wrap">
                        <Badge variant="secondary" className="text-xs">
                          {member.role}
                        </Badge>
                        {member.email && (
                          <span className="text-xs text-slate-400 truncate">{member.email}</span>
                        )}
                        <span className="text-xs text-slate-400">
                          Invite: {formatDate(member.invite_sent_at)}
                        </span>
                      </div>
                    </div>

                    <div className="flex items-center gap-2 shrink-0">
                      <div className="flex flex-col items-end gap-0.5">
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() => handleSendInvite(member)}
                          disabled={sendingInviteId === member.user_id || !merchantInviteLinkSet}
                          title={
                            !merchantInviteLinkSet
                              ? 'Set this merchant\'s invite link first'
                              : 'Re-send invite email'
                          }
                        >
                          <Send className="h-3.5 w-3.5 mr-1" />
                          {sendingInviteId === member.user_id ? 'Sending…' : 'Invite'}
                        </Button>
                        {!merchantInviteLinkSet && (
                          <span className="text-xs text-amber-600 leading-tight text-right">
                            Set this merchant's invite link first
                          </span>
                        )}
                      </div>
                      <Select
                        value={getScopeForMember(member.user_id)}
                        onValueChange={(v) =>
                          setScopeForMember(member.user_id, v as 'channel' | 'server')
                        }
                      >
                        <SelectTrigger className="w-28 h-8 text-xs">
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="channel">Channel</SelectItem>
                          <SelectItem value="server">Server</SelectItem>
                        </SelectContent>
                      </Select>
                      <Button
                        variant="destructive"
                        size="sm"
                        onClick={() => handleExpel(member)}
                        disabled={expellingId === member.user_id}
                      >
                        {expellingId === member.user_id ? 'Expelling…' : 'Expel'}
                      </Button>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </ScrollArea>
        )}
      </DialogContent>
    </Dialog>
    </>
  )
}
