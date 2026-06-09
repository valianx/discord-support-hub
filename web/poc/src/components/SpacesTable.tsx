import { useState } from 'react'
import { MoreHorizontal, RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { type ApiConfig, type Space, type SpaceLifecycleState, type AclState } from '@/lib/api'
import { InviteCollaboratorDialog } from '@/components/InviteCollaboratorDialog'
import { MembersDialog } from '@/components/MembersDialog'
import { LifecycleDialog } from '@/components/LifecycleDialog'

interface SpacesTableProps {
  spaces: Space[]
  loading: boolean
  loadError: boolean
  apiConfig: ApiConfig
  onRefresh: () => void
  onChanged: () => void
}

function lifecycleBadgeVariant(state: SpaceLifecycleState) {
  switch (state) {
    case 'active': return 'success' as const
    case 'resolved': return 'warning' as const
    case 'archived': return 'secondary' as const
  }
}

function aclBadgeVariant(state: AclState) {
  switch (state) {
    case 'applied': return 'success' as const
    case 'pending': return 'warning' as const
    case 'degraded': return 'warning' as const
    case 'failed': return 'destructive' as const
  }
}

function formatDate(iso: string | null | undefined): string {
  if (!iso) return '—'
  return new Intl.DateTimeFormat('en', {
    dateStyle: 'short',
    timeStyle: 'short',
  }).format(new Date(iso))
}

export function SpacesTable({
  spaces,
  loading,
  loadError,
  apiConfig,
  onRefresh,
  onChanged,
}: SpacesTableProps) {
  const [inviteSpace, setInviteSpace] = useState<Space | null>(null)
  const [membersSpace, setMembersSpace] = useState<Space | null>(null)
  const [lifecycleSpace, setLifecycleSpace] = useState<Space | null>(null)

  return (
    <div>
      <div className="flex items-center justify-between mb-3">
        <h2 className="text-lg font-semibold">Spaces</h2>
        <Button
          variant="outline"
          size="sm"
          onClick={onRefresh}
          disabled={loading}
        >
          <RefreshCw className={`h-4 w-4 mr-2 ${loading ? 'animate-spin' : ''}`} />
          Refresh
        </Button>
      </div>

      {loading && spaces.length === 0 && (
        <p className="text-sm text-slate-500 py-8 text-center">Loading spaces…</p>
      )}

      {!loading && spaces.length === 0 && loadError && (
        <p className="text-sm text-red-500 py-8 text-center">
          Could not load spaces. Check the error notification for details, then refresh.
        </p>
      )}

      {!loading && spaces.length === 0 && !loadError && (
        <p className="text-sm text-slate-500 py-8 text-center">
          No spaces found. Provision one to get started.
        </p>
      )}

      {spaces.length > 0 && (
        <div className="rounded-md border border-slate-200">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Lifecycle</TableHead>
                <TableHead>ACL</TableHead>
                <TableHead>Merchant</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Last Activity</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {spaces.map((space) => (
                <TableRow key={space.id}>
                  <TableCell className="font-medium">
                    {space.name}
                    {space.discord_channel_id && (
                      <div className="text-xs text-slate-400 mt-0.5 font-normal">
                        #{space.discord_channel_id}
                      </div>
                    )}
                  </TableCell>
                  <TableCell>
                    <Badge variant={lifecycleBadgeVariant(space.lifecycle_state)}>
                      {space.lifecycle_state}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <Badge variant={aclBadgeVariant(space.acl_state)}>
                      {space.acl_state}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <code className="text-xs bg-slate-50 px-1 py-0.5 rounded">
                      {space.merchant_id.slice(0, 8)}…
                    </code>
                  </TableCell>
                  <TableCell className="text-sm">{formatDate(space.created_at)}</TableCell>
                  <TableCell className="text-sm">{formatDate(space.last_activity_at)}</TableCell>
                  <TableCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button variant="ghost" size="icon" className="h-8 w-8">
                          <MoreHorizontal className="h-4 w-4" />
                          <span className="sr-only">Open menu</span>
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuLabel>Actions</DropdownMenuLabel>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem onClick={() => setMembersSpace(space)}>
                          View Members
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => setInviteSpace(space)}>
                          Invite Collaborator
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem onClick={() => setLifecycleSpace(space)}>
                          Change Lifecycle
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {inviteSpace && (
        <InviteCollaboratorDialog
          open={true}
          onOpenChange={(open) => !open && setInviteSpace(null)}
          spaceId={inviteSpace.id}
          spaceName={inviteSpace.name}
          apiConfig={apiConfig}
        />
      )}

      {membersSpace && (
        <MembersDialog
          open={true}
          onOpenChange={(open) => !open && setMembersSpace(null)}
          spaceId={membersSpace.id}
          spaceName={membersSpace.name}
          apiConfig={apiConfig}
        />
      )}

      {lifecycleSpace && (
        <LifecycleDialog
          open={true}
          onOpenChange={(open) => !open && setLifecycleSpace(null)}
          space={lifecycleSpace}
          apiConfig={apiConfig}
          onChanged={() => {
            setLifecycleSpace(null)
            onChanged()
          }}
        />
      )}
    </div>
  )
}
