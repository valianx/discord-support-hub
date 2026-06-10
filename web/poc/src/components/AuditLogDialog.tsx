import { useState, useEffect, useCallback } from 'react'
import { RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Badge } from '@/components/ui/badge'
import { type ApiConfig, type AuditEntry, getAudit, ApiError } from '@/lib/api'
import { toast } from 'sonner'

interface AuditLogDialogProps {
  apiConfig: ApiConfig
}

function formatDate(iso: string): string {
  return new Intl.DateTimeFormat('en', {
    dateStyle: 'short',
    timeStyle: 'short',
  }).format(new Date(iso))
}

function actionBadgeVariant(action: string): 'default' | 'secondary' | 'destructive' | 'warning' {
  if (action.includes('expel') || action.includes('remove') || action.includes('revoke')) {
    return 'destructive'
  }
  if (action.includes('provision') || action.includes('register') || action.includes('add')) {
    return 'default'
  }
  if (action.includes('archive') || action.includes('repair')) {
    return 'warning'
  }
  return 'secondary'
}

export function AuditLogDialog({ apiConfig }: AuditLogDialogProps) {
  const [open, setOpen] = useState(false)
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [loading, setLoading] = useState(false)

  const loadAudit = useCallback(async () => {
    setLoading(true)
    try {
      const result = await getAudit(apiConfig, { limit: 50 })
      setEntries(result.items)
    } catch (err) {
      if (err instanceof ApiError) {
        toast.error(`[${err.code}] ${err.message}`)
      } else {
        const message = err instanceof Error ? err.message : String(err)
        toast.error(`Failed to load audit log: ${message}`)
      }
    } finally {
      setLoading(false)
    }
  }, [apiConfig])

  useEffect(() => {
    if (!open) return
    // Data-fetching effect — intentionally calls setState inside async work.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void loadAudit()
  }, [open, loadAudit])

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">
          Audit Log
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Audit Log</DialogTitle>
          <DialogDescription>
            Recent provisioning, membership, lifecycle, and expulsion events (last 50).
          </DialogDescription>
        </DialogHeader>

        <div className="flex justify-end mb-1">
          <Button
            variant="ghost"
            size="sm"
            onClick={loadAudit}
            disabled={loading}
          >
            <RefreshCw className={`h-4 w-4 mr-1 ${loading ? 'animate-spin' : ''}`} />
            Refresh
          </Button>
        </div>

        {loading && (
          <p className="text-sm text-slate-500 py-4 text-center">Loading…</p>
        )}

        {!loading && entries.length === 0 && (
          <p className="text-sm text-slate-500 py-4 text-center">No audit entries found.</p>
        )}

        {!loading && entries.length > 0 && (
          <ScrollArea className="max-h-[480px]">
            <div className="grid gap-1.5">
              {entries.map((entry) => (
                <div
                  key={entry.id}
                  className="flex items-start gap-3 rounded-md border border-slate-100 bg-slate-50 px-3 py-2 text-sm"
                >
                  <div className="shrink-0 mt-0.5">
                    <Badge variant={actionBadgeVariant(entry.action)} className="text-xs font-mono">
                      {entry.action}
                    </Badge>
                  </div>
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2 flex-wrap text-xs text-slate-500">
                      {entry.space_id && (
                        <span>space: {entry.space_id.slice(0, 8)}…</span>
                      )}
                      {entry.merchant_id && (
                        <span>merchant: {entry.merchant_id.slice(0, 8)}…</span>
                      )}
                      {entry.target_user_id && (
                        <span>user: {entry.target_user_id.slice(0, 8)}…</span>
                      )}
                      {entry.scope && (
                        <span>scope: {entry.scope}</span>
                      )}
                    </div>
                  </div>
                  <div className="shrink-0 text-xs text-slate-400 whitespace-nowrap">
                    {formatDate(entry.created_at)}
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
