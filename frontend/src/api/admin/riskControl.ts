import { apiClient } from '../client'

export type ModerationMode = 'off' | 'pre_block'
export type ContentModerationLayerStage = 'enforce' | 'shadow'
export type ContentModerationFirstLayerStage = ContentModerationLayerStage
export type ContentModerationSecondLayerStage = ContentModerationLayerStage
export type ContentModerationModelFilterType = 'all' | 'include' | 'exclude'
export type ContentModerationRemoteProvider = 'deepseek' | 'alibaba_qwen' | 'zhipu_glm' | 'mimo'
export type ContentModerationRemoteUnavailablePolicy = 'fail_closed' | 'risk_tiered'
export type DeepSeekBreakerState =
  | 'closed'
  | 'cooldown'
  | 'half_open'
  | 'auth_disabled'
  | 'open'
  | 'disabled'
  | 'unknown'

export interface ContentModerationModelFilter {
  type: ContentModerationModelFilterType
  models: string[]
}

export interface DeepSeekModerationChannel {
  id: string
  name: string
  provider?: ContentModerationRemoteProvider | string
  base_url: string
  model: string
  enabled: boolean
  order: number
  timeout_ms: number
  api_key_configured: boolean
  api_key_masked: string
  health_status?: string
  last_health_checked_at?: string
  breaker_status?: DeepSeekBreakerState
  cooldown_until?: string
  last_latency_ms?: number
  last_error?: string
  /** Last cheap transport-only heartbeat performed by the backend. */
  heartbeat_status?: 'reachable' | 'unreachable' | 'untested' | 'disabled' | string
  last_heartbeat_at?: string
  heartbeat_latency_ms?: number
  heartbeat_http_status?: number
  heartbeat_error?: string
}

export interface UpdateDeepSeekModerationChannel {
  id: string
  name: string
  provider?: ContentModerationRemoteProvider | string
  base_url: string
  model: string
  enabled: boolean
  order: number
  timeout_ms: number
  api_key?: string
  clear_api_key?: boolean
}

export interface TestDeepSeekChannelResponse {
  channel_id: string
  provider?: ContentModerationRemoteProvider | string
  model?: string
  /** `api_usability` for the explicit real review test. */
  test_type?: 'api_usability' | string
  reachable: boolean
  /** @deprecated Compatibility alias for reachable. */
  health_valid: boolean
  latency_ms: number
  http_status?: number
  verdict?: 'safe' | 'restricted' | 'violation' | string
  category?: string
  confidence?: number
  error?: string
  checked_at?: string
}

export interface ContentModerationChannelHeartbeat {
  channel_id: string
  provider: ContentModerationRemoteProvider | string
  status: 'reachable' | 'unreachable' | 'untested' | 'disabled' | string
  checked_at?: string
  latency_ms?: number
  http_status?: number
  error?: string
}

export interface ContentModerationEndpoint {
  id: string
  name: string
  base_url: string
  model: string
  profile: 'yufeng_xguard'
  enabled: boolean
  timeout_ms: number
  input_limit: number
  token_configured: boolean
  token_masked: string
}

export interface ContentModerationConfig {
  enabled: boolean
  mode: ModerationMode
  deepseek_enabled?: boolean
  remote_reviewers_enabled?: boolean
  remote_consensus_required?: number
  remote_unavailable_policy?: ContentModerationRemoteUnavailablePolicy
  yufeng_enabled?: boolean
  yufeng_mode?: 'shadow' | string
  deepseek_total_timeout_ms?: number
  deepseek_threshold?: number
  policy_version?: string
  deepseek_channels?: DeepSeekModerationChannel[]
  remote_reviewers?: DeepSeekModerationChannel[]
  all_groups?: boolean
  group_ids?: number[]
  user_email_whitelist?: string[]
  record_non_hits?: boolean
  block_status?: number
  block_message?: string
  email_on_hit?: boolean
  auto_ban_enabled?: boolean
  cyber_policy_exclude_from_ban_count?: boolean
  ban_threshold?: number
  violation_window_hours?: number
  hit_retention_days?: number
  non_hit_retention_days?: number
  model_filter?: ContentModerationModelFilter
  first_layer_stage?: ContentModerationFirstLayerStage
  second_layer_enabled?: boolean
  second_layer_stage?: ContentModerationSecondLayerStage
  second_layer_endpoints?: ContentModerationEndpoint[]
  layer1_keywords?: string[]
  layer2_keywords?: string[]
  keyword_allowlist?: string[]
  keyword_policy_version?: string
  context_policy_version?: string
  evidence_policy_version?: string
  candidate_layer1_count?: number
  candidate_layer2_count?: number
  candidate_system_ready?: boolean
  candidate_system_error?: string
}

export interface UpdateContentModerationConfig {
  enabled?: boolean
  mode?: ModerationMode
  deepseek_enabled?: boolean
  remote_reviewers_enabled?: boolean
  remote_consensus_required?: number
  remote_unavailable_policy?: ContentModerationRemoteUnavailablePolicy
  yufeng_enabled?: boolean
  yufeng_mode?: 'shadow' | string
  deepseek_total_timeout_ms?: number
  deepseek_threshold?: number
  deepseek_channels?: UpdateDeepSeekModerationChannel[]
  remote_reviewers?: UpdateDeepSeekModerationChannel[]
  all_groups?: boolean
  group_ids?: number[]
  user_email_whitelist?: string[]
  record_non_hits?: boolean
  block_status?: number
  block_message?: string
  email_on_hit?: boolean
  auto_ban_enabled?: boolean
  cyber_policy_exclude_from_ban_count?: boolean
  ban_threshold?: number
  violation_window_hours?: number
  hit_retention_days?: number
  non_hit_retention_days?: number
  model_filter?: ContentModerationModelFilter
  first_layer_stage?: ContentModerationFirstLayerStage
  second_layer_enabled?: boolean
  second_layer_stage?: ContentModerationSecondLayerStage
  layer1_keywords?: string[]
  layer2_keywords?: string[]
  keyword_allowlist?: string[]
}

export interface ContentModerationArchiveRuntimeStatus {
  degraded?: boolean
  retry_queue_depth?: number
  emergency_queue_depth?: number
  archive_retry_attempts?: number
  archive_retry_errors?: number
  content_lost?: number
  disk_free_bytes?: number
  disposition_queue_depth?: number
  disposition_retry_attempts?: number
  disposition_retry_errors?: number
  lost_summary_queue_depth?: number
}

export interface ContentModerationRuntimeStatus {
  enabled: boolean
  risk_control_enabled: boolean
  mode: ModerationMode
  pre_block_active?: number
  pre_block_checked?: number
  pre_block_allowed?: number
  pre_block_blocked?: number
  pre_block_errors?: number
  pre_block_avg_latency_ms?: number
  flagged_hash_count?: number
  last_cleanup_at?: string
  last_cleanup_deleted_hit?: number
  last_cleanup_deleted_non_hit?: number
  last_cleanup_deleted_archives?: number
  deepseek_selected_count?: number
  deepseek_failover_count?: number
  deepseek_unavailable_count?: number
  remote_selected_count?: number
  remote_failover_count?: number
  remote_unavailable_count?: number
  deepseek_response_read_timeout_count?: number
  deepseek_breaker_skip_count?: number
  deepseek_cooldown_skip_count?: number
  deepseek_half_open_busy_skip_count?: number
  review_unavailable_count?: number
  review_unavailable_enforced_count?: number
  review_unavailable_degraded_count?: number
  last_review_unavailable_at?: string
  startup_api_usability_tested?: boolean
  startup_api_usability_checked_at?: string
  startup_api_usability_configured?: number
  startup_api_usability_succeeded?: number
  remote_heartbeats?: ContentModerationChannelHeartbeat[]
  second_layer_cache_hits?: number
  second_layer_cache_misses?: number
  second_layer_cache_writes?: number
  second_layer_cache_errors?: number
  second_layer_enforce_ready?: boolean
  second_layer_enforce_reason?: string
  archive_runtime?: ContentModerationArchiveRuntimeStatus
}

export interface ContentModerationEvidenceMatch {
  keyword: string
  rule_id: string
  tier: string
  start: number
  end: number
}

export interface ContentModerationEvidenceWindow {
  path: string
  context_class: string
  text: string
  matches: ContentModerationEvidenceMatch[]
}

export interface DeepSeekReviewAttempt {
  reviewer?: string
  provider?: string
  channel_id?: string
  channel_name?: string
  model?: string
  outcome?: string
  verdict?: string
  role?: string
  confidence?: number
  http_status?: number
  latency_ms?: number
  error?: string
}

export interface ContentModerationLog {
  id: number
  created_at: string
  request_id?: string
  user_id?: number | null
  user_email?: string
  api_key_id?: number | null
  api_key_name?: string
  group_id?: number | null
  group_name?: string
  endpoint?: string
  provider?: string
  model?: string
  mode?: string
  action?: string
  cache_hit?: boolean
  decision_source?: string
  source_log_id?: number | null
  replay_of_input_hash?: string
  context_class?: string
  model_profile?: string
  policy_version?: string
  evidence_policy_version?: string
  keyword_tier?: string
  keyword_rule_id?: string
  evidence_truncated?: boolean
  evidence_windows?: ContentModerationEvidenceWindow[]
  parser_status?: string
  flagged?: boolean
  highest_category?: string
  highest_score?: number
  matched_keyword?: string
  input_excerpt?: string
  upstream_latency_ms?: number | null
  error?: string
  violation_count?: number
  auto_banned?: boolean
  email_sent?: boolean
  user_status?: string
  queue_delay_ms?: number | null
  input_hash?: string
  archive_id?: string
  archive_bytes?: number
  archive_status?: string
  archive_incomplete?: boolean
  archive_content_lost?: boolean
  archive_deleted_at?: string
  deepseek_confidence?: number | null
  deepseek_category?: string
  deepseek_reason?: string
  review_outcome?: string
  reviewer_disagreement?: boolean
  review_attempts?: DeepSeekReviewAttempt[]
}

export type ContentModerationLogResult =
  | 'blocked'
  | 'hit'
  | 'pass'
  | 'error'
  | 'cyber_policy'
  | 'content_blocked'
  | 'violation_blocked'
  | 'restricted'
  | 'risky_shadow'
  | 'review_unavailable'
  | 'evidence_capacity_exceeded'

export type ContentModerationLogView =
  | 'cyber_policy'
  | 'violation_blocked'
  | 'restricted'
  | 'risky_shadow'
  | 'review_unavailable'
  | 'evidence_capacity_exceeded'

export interface ListContentModerationLogsParams {
  page?: number
  page_size?: number
  log_id?: number
  result?: ContentModerationLogResult
  group_id?: number
  endpoint?: string
  context_class?: string
  model_profile?: string
  decision_source?: string
  search?: string
  from?: string
  to?: string
}

export interface ContentModerationLogsResponse {
  items: ContentModerationLog[]
  total: number
  page: number
  page_size: number
  pages: number
}

export interface ContentModerationArchivePreview {
  content: string
  returned_bytes: number
  total_bytes: number
  truncated: boolean
}

export interface DeleteContentModerationArchiveResponse {
  deleted: boolean
}

export interface ContentModerationUnbanUserResponse {
  user_id: number
  status: string
}

export interface DeleteFlaggedHashResponse {
  input_hash: string
  deleted: boolean
  cache_version: string
  cache_namespace: string
}

export interface ClearFlaggedHashesResponse {
  deleted: number
  cache_version: string
  cache_namespace: string
}

export async function getConfig(): Promise<ContentModerationConfig> {
  const { data } = await apiClient.get<ContentModerationConfig>('/admin/risk-control/config')
  return data
}

export async function updateConfig(payload: UpdateContentModerationConfig): Promise<ContentModerationConfig> {
  const { data } = await apiClient.put<ContentModerationConfig>('/admin/risk-control/config', payload)
  return data
}

export async function getStatus(): Promise<ContentModerationRuntimeStatus> {
  const { data } = await apiClient.get<ContentModerationRuntimeStatus>('/admin/risk-control/status')
  return data
}

export async function testDeepSeekChannel(channelID: string): Promise<TestDeepSeekChannelResponse> {
  const { data } = await apiClient.post<TestDeepSeekChannelResponse>(
    `/admin/risk-control/deepseek/channels/${encodeURIComponent(channelID)}/test`
  )
  return data
}

/** Explicit real moderation request used by the admin "test API" action. */
export async function testAPIAvailability(channelID: string): Promise<TestDeepSeekChannelResponse> {
  const { data } = await apiClient.post<TestDeepSeekChannelResponse>(
    `/admin/risk-control/deepseek/channels/${encodeURIComponent(channelID)}/test-api`
  )
  return data
}

export async function listLogs(params: ListContentModerationLogsParams = {}): Promise<ContentModerationLogsResponse> {
  const { data } = await apiClient.get<ContentModerationLogsResponse>('/admin/risk-control/logs', {
    params,
  })
  return data
}

export async function getLog(logID: number): Promise<ContentModerationLog> {
  const { data } = await apiClient.get<ContentModerationLog>(`/admin/risk-control/logs/${logID}`)
  return data
}

export async function previewArchive(logID: number): Promise<ContentModerationArchivePreview> {
  const { data } = await apiClient.get<ContentModerationArchivePreview>(
    `/admin/risk-control/logs/${logID}/archive/preview`
  )
  return data
}

export async function downloadArchive(logID: number): Promise<Blob> {
  const { data } = await apiClient.get<Blob>(`/admin/risk-control/logs/${logID}/archive/download`, {
    responseType: 'blob',
  })
  return data
}

export async function deleteArchive(logID: number): Promise<DeleteContentModerationArchiveResponse> {
  const { data } = await apiClient.delete<DeleteContentModerationArchiveResponse>(
    `/admin/risk-control/logs/${logID}/archive`
  )
  return data
}

export async function unbanUser(userID: number): Promise<ContentModerationUnbanUserResponse> {
  const { data } = await apiClient.post<ContentModerationUnbanUserResponse>(`/admin/risk-control/users/${userID}/unban`)
  return data
}

export async function deleteFlaggedHash(inputHash: string): Promise<DeleteFlaggedHashResponse> {
  const { data } = await apiClient.delete<DeleteFlaggedHashResponse>('/admin/risk-control/hashes', {
    data: { input_hash: inputHash },
  })
  return data
}

export async function clearFlaggedHashes(): Promise<ClearFlaggedHashesResponse> {
  const { data } = await apiClient.delete<ClearFlaggedHashesResponse>('/admin/risk-control/hashes/all')
  return data
}

export const riskControlAPI = {
  getConfig,
  updateConfig,
  getStatus,
  testDeepSeekChannel,
  testAPIAvailability,
  listLogs,
  getLog,
  previewArchive,
  downloadArchive,
  deleteArchive,
  unbanUser,
  deleteFlaggedHash,
  clearFlaggedHashes,
}

export default riskControlAPI
