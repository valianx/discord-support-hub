import { useState, useEffect, useCallback } from 'react'
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
  pollJob,
} from '@/lib/api'
import { toast } from 'sonner'

interface MembersDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  spaceId: string
  spaceName: string
  apiConfig: ApiConfig
}

export function MembersDialog({
  open,
  onOpenChange,
  spaceId,
  spaceName,
  apiConfig,
}: MembersDialogProps) {
  const [members, setMembers] = useState<SpaceMember[]>([])
  const [loading, setLoading] = useState(false)
  const [expellingId, setExpellingId] = useState<string | null>(null)
  const [expelScopes, setExpelScopes] = useState<Record<string, 'channel' | 'server'>>({})

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

  async function handleExpel(member: SpaceMember) {
    const scope = getScopeForMember(member.user_id)
    setExpellingId(member.user_id)

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

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Members — {spaceName}</DialogTitle>
          <DialogDescription>
            Collaborators with access to this space.
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
                  className="flex items-center justify-between rounded-md border border-slate-200 px-3 py-2"
                >
                  <div className="flex flex-col gap-0.5 min-w-0">
                    <span className="text-sm font-medium truncate">
                      {member.display_name ?? member.discord_user_id ?? member.user_id.slice(0, 8) + '…'}
                    </span>
                    <div className="flex items-center gap-2">
                      <Badge variant="secondary" className="text-xs">
                        {member.role}
                      </Badge>
                      {member.overwrite_applied ? (
                        <Badge variant="success" className="text-xs">ACL applied</Badge>
                      ) : (
                        <Badge variant="warning" className="text-xs">ACL pending</Badge>
                      )}
                    </div>
                  </div>

                  <div className="flex items-center gap-2 shrink-0">
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
              ))}
            </div>
          </ScrollArea>
        )}
      </DialogContent>
    </Dialog>
  )
}
