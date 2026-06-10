// Typed API client for the Discord Support Hub control-plane API.
// Base URL and bearer token are runtime-supplied (never hardcoded).

export interface HubError {
  code: string
  message: string
  details?: Record<string, unknown> | null
}

export class ApiError extends Error {
  readonly code: string
  readonly status: number
  readonly details: Record<string, unknown> | null | undefined

  constructor(status: number, body: HubError) {
    super(body.message)
    this.code = body.code
    this.status = status
    this.details = body.details
  }
}

export type SpaceLifecycleState = 'active' | 'resolved' | 'archived'
export type AclState = 'pending' | 'applied' | 'degraded' | 'failed'
export type JobStatus = 'pending' | 'active' | 'completed' | 'retrying' | 'archived'

export interface Space {
  id: string
  merchant_id: string
  name: string
  discord_channel_id: string | null
  discord_category_id: string | null
  lifecycle_state: SpaceLifecycleState
  acl_state: AclState
  last_activity_at: string | null
  created_at: string
  archived_at: string | null
}

export interface SpaceMember {
  user_id: string
  discord_user_id: string | null
  display_name: string | null
  merchant_id: string | null
  role: 'collaborator'
  overwrite_applied: boolean
  invited_by: string | null
  created_at: string
}

export interface Job {
  id: string
  kind: string
  status: JobStatus
  space_id: string | null
  merchant_id: string | null
  user_id: string | null
  retry_count: number
  error: string | null
  created_at: string
  completed_at: string | null
}

export interface JobAcceptedBody {
  job: Job
}

export interface InviteCollaboratorResponse extends JobAcceptedBody {
  connect_url?: string | null
}

export interface ProvisionSpaceRequest {
  name: string
  category_id?: string | null
  welcome_message?: string | null
}

export interface InviteCollaboratorRequest {
  user_id?: string | null
  discord_user_id?: string | null
  email?: string | null
  display_name?: string | null
}

export interface LifecycleAction {
  action: 'open' | 'resolve' | 'archive' | 'reopen'
}

export interface Merchant {
  id: string
  external_ref: string
  name: string
  help_desk_url: string | null
  is_active: boolean
  created_at: string
}

export interface RegisterMerchantRequest {
  external_ref: string
  name: string
  help_desk_url?: string | null
}

export interface ListMerchantsResponse {
  items: Merchant[]
  next_cursor?: string | null
}

export interface ListSpacesResponse {
  items: Space[]
  next_cursor?: string | null
}

export interface ListMembersResponse {
  items: SpaceMember[]
}

export interface ReadyzResponse {
  postgres: string
  valkey: string
}

// -------------------------------------------------------------------------
// Client factory — creates an API client bound to a base URL and bearer key.
// -------------------------------------------------------------------------

export interface ApiConfig {
  baseUrl: string
  apiKey: string
}

async function request<T>(
  config: ApiConfig,
  method: string,
  path: string,
  body?: unknown,
  extraHeaders?: Record<string, string>
): Promise<T> {
  const url = `${config.baseUrl.replace(/\/$/, '')}${path}`
  const headers: Record<string, string> = {
    Authorization: `Bearer ${config.apiKey}`,
    ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
    ...extraHeaders,
  }

  const response = await fetch(url, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })

  if (!response.ok) {
    let errorBody: HubError
    try {
      errorBody = (await response.json()) as HubError
    } catch {
      errorBody = { code: 'unknown_error', message: `HTTP ${response.status}` }
    }
    throw new ApiError(response.status, errorBody)
  }

  // 204 No Content
  if (response.status === 204) {
    return undefined as T
  }

  return response.json() as Promise<T>
}

// -------------------------------------------------------------------------
// Readiness check (used for connection status in the header).
// -------------------------------------------------------------------------

export async function checkReadiness(baseUrl: string): Promise<ReadyzResponse> {
  const url = `${baseUrl.replace(/\/$/, '')}/readyz`
  const response = await fetch(url)
  if (!response.ok) {
    throw new Error(`Readiness check failed: ${response.status}`)
  }
  return response.json() as Promise<ReadyzResponse>
}

// -------------------------------------------------------------------------
// Merchants
// -------------------------------------------------------------------------

export function registerMerchant(
  config: ApiConfig,
  body: RegisterMerchantRequest,
  idempotencyKey?: string
): Promise<Merchant> {
  return request<Merchant>(
    config,
    'POST',
    '/v1/merchants',
    body,
    idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}
  )
}

export function listMerchants(
  config: ApiConfig,
  params?: { cursor?: string; is_active?: boolean }
): Promise<ListMerchantsResponse> {
  const qs = new URLSearchParams()
  if (params?.cursor) qs.set('cursor', params.cursor)
  if (params?.is_active !== undefined) qs.set('is_active', String(params.is_active))
  const query = qs.toString() ? `?${qs.toString()}` : ''
  return request<ListMerchantsResponse>(config, 'GET', `/v1/merchants${query}`)
}

// -------------------------------------------------------------------------
// Spaces
// -------------------------------------------------------------------------

export function listSpaces(
  config: ApiConfig,
  params?: { lifecycle_state?: SpaceLifecycleState; limit?: number }
): Promise<ListSpacesResponse> {
  const qs = new URLSearchParams()
  if (params?.lifecycle_state) qs.set('lifecycle_state', params.lifecycle_state)
  if (params?.limit) qs.set('limit', String(params.limit))
  const query = qs.toString() ? `?${qs.toString()}` : ''
  return request<ListSpacesResponse>(config, 'GET', `/v1/channels${query}`)
}

export function provisionSpace(
  config: ApiConfig,
  merchantId: string,
  body: ProvisionSpaceRequest,
  idempotencyKey?: string
): Promise<JobAcceptedBody> {
  return request<JobAcceptedBody>(
    config,
    'POST',
    `/v1/merchants/${merchantId}/channels`,
    body,
    idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}
  )
}

export function changeLifecycle(
  config: ApiConfig,
  spaceId: string,
  body: LifecycleAction,
  idempotencyKey?: string
): Promise<JobAcceptedBody> {
  return request<JobAcceptedBody>(
    config,
    'POST',
    `/v1/channels/${spaceId}/lifecycle`,
    body,
    idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}
  )
}

export function listMembers(
  config: ApiConfig,
  spaceId: string
): Promise<ListMembersResponse> {
  return request<ListMembersResponse>(config, 'GET', `/v1/channels/${spaceId}/members`)
}

// -------------------------------------------------------------------------
// Collaborators
// -------------------------------------------------------------------------

export function inviteCollaborator(
  config: ApiConfig,
  spaceId: string,
  body: InviteCollaboratorRequest,
  idempotencyKey?: string
): Promise<InviteCollaboratorResponse> {
  return request<InviteCollaboratorResponse>(
    config,
    'POST',
    `/v1/channels/${spaceId}/collaborators`,
    body,
    idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}
  )
}

export function expelCollaborator(
  config: ApiConfig,
  spaceId: string,
  userId: string,
  scope: 'channel' | 'server' = 'channel',
  idempotencyKey?: string
): Promise<JobAcceptedBody> {
  return request<JobAcceptedBody>(
    config,
    'DELETE',
    `/v1/channels/${spaceId}/collaborators/${userId}?scope=${scope}`,
    undefined,
    idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}
  )
}

// -------------------------------------------------------------------------
// Jobs
// -------------------------------------------------------------------------

export function getJob(config: ApiConfig, jobId: string): Promise<Job> {
  return request<Job>(config, 'GET', `/v1/jobs/${jobId}`)
}

// -------------------------------------------------------------------------
// Job polling helper — polls until completed/archived or timeout.
// -------------------------------------------------------------------------

export async function pollJob(
  config: ApiConfig,
  jobId: string,
  onUpdate: (job: Job) => void,
  intervalMs = 2000,
  maxAttempts = 30
): Promise<Job> {
  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    const job = await getJob(config, jobId)
    onUpdate(job)
    if (job.status === 'completed' || job.status === 'archived') {
      return job
    }
    await new Promise<void>((resolve) => setTimeout(resolve, intervalMs))
  }
  throw new Error(`Job ${jobId} did not complete within the polling window`)
}
