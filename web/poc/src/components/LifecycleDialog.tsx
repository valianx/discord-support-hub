import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
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
import { Label } from '@/components/ui/label'
import {
  type ApiConfig,
  type Space,
  type SpaceLifecycleState,
  changeLifecycle,
  pollJob,
} from '@/lib/api'
import { toast } from 'sonner'

type LifecycleAction = 'open' | 'resolve' | 'archive' | 'reopen'

const VALID_TRANSITIONS: Record<SpaceLifecycleState, LifecycleAction[]> = {
  active: ['resolve', 'archive'],
  resolved: ['archive', 'reopen'],
  archived: ['reopen'],
}

interface LifecycleDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  space: Space
  apiConfig: ApiConfig
  onChanged: () => void
}

export function LifecycleDialog({
  open,
  onOpenChange,
  space,
  apiConfig,
  onChanged,
}: LifecycleDialogProps) {
  const validActions = VALID_TRANSITIONS[space.lifecycle_state] ?? []
  const [action, setAction] = useState<LifecycleAction | ''>(validActions[0] ?? '')
  const [loading, setLoading] = useState(false)

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!action) return

    setLoading(true)
    try {
      const result = await changeLifecycle(apiConfig, space.id, { action })
      toast.info(`Lifecycle job accepted: ${result.job.id.slice(0, 8)}…`)

      const finalJob = await pollJob(apiConfig, result.job.id, () => {})

      if (finalJob.status === 'completed') {
        toast.success(`Space ${action}d successfully`)
        onChanged()
        onOpenChange(false)
      } else {
        toast.error(`Lifecycle change failed: ${finalJob.error ?? 'unknown'}`)
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err)
      toast.error(`Error: ${message}`)
    } finally {
      setLoading(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Change Lifecycle — {space.name}</DialogTitle>
          <DialogDescription>
            Current state:{' '}
            <Badge variant="outline" className="ml-1">
              {space.lifecycle_state}
            </Badge>
          </DialogDescription>
        </DialogHeader>

        {validActions.length === 0 ? (
          <p className="text-sm text-slate-500">
            No transitions available from the current state.
          </p>
        ) : (
          <form onSubmit={handleSubmit}>
            <div className="grid gap-4 py-2">
              <div className="grid gap-2">
                <Label htmlFor="lifecycle-action">Action</Label>
                <Select
                  value={action}
                  onValueChange={(v) => setAction(v as LifecycleAction)}
                >
                  <SelectTrigger id="lifecycle-action">
                    <SelectValue placeholder="Select action" />
                  </SelectTrigger>
                  <SelectContent>
                    {validActions.map((a) => (
                      <SelectItem key={a} value={a}>
                        {a.charAt(0).toUpperCase() + a.slice(1)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>
            <DialogFooter className="mt-4">
              <Button type="submit" disabled={loading || !action}>
                {loading ? 'Applying…' : 'Apply'}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}
