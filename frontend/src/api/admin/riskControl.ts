import { apiClient } from '../client'

export type ModerationMode = 'off' | 'pre_block'
export type KeywordBlockingMode = 'keyword_only' | 'keyword_and_api' | 'api_only'
export type ContentModerationModelProfile = 'qwen_guard' | 'yufeng_xguard'
export type ContentModerationSecondLayerStage = 'enforce' | 'shadow'
export type ContentModerationModelFilterType = 'all' | 'include' | 'exclude'

export interface ContentModerationModelFilter {
  type: ContentModerationModelFilterType
  models: string[]
}

export interface ContentModerationEndpoint {
  id: string
  name: string
  base_url: string
  model: string
  profile: ContentModerationModelProfile
  model_revision?: string
  prompt_version?: string
  stop_tokens?: string[]
  enabled: boolean
  timeout_ms: number
  input_limit: number
  token_configured: boolean
  token_masked: string
}

export interface ContentModerationConfig {
  enabled: boolean
  mode: ModerationMode
  base_url: string
  model: string
  proxy_id: number | null
  api_key_configured: boolean
  api_key_masked: string
  api_key_count: number
  api_key_masks: string[]
  api_key_statuses: ContentModerationAPIKeyStatus[]
  timeout_ms: number
  sample_rate: number
  all_groups: boolean
  group_ids: number[]
  user_email_whitelist: string[]
  record_non_hits: boolean
  thresholds: Record<string, number>
  block_status: number
  block_message: string
  email_on_hit: boolean
  auto_ban_enabled: boolean
  ban_threshold: number
  violation_window_hours: number
  retry_count: number
  hit_retention_days: number
  non_hit_retention_days: number
  pre_hash_check_enabled: boolean
  blocked_keywords: string[]
  keyword_blocking_mode: KeywordBlockingMode
  model_filter: ContentModerationModelFilter
  cache_version: string
  cache_max_entries: number
  cache_max_bytes: number
  fragment_block_ttl_seconds: number
  fragment_allow_ttl_seconds: number
  fragment_ttl_policy_version: string
  second_layer_enabled: boolean
  second_layer_stage: ContentModerationSecondLayerStage
  second_layer_endpoints: ContentModerationEndpoint[]
  second_layer_scanners: string[]
  hard_block_patterns: string[]
  candidate_keywords: string[]
  keyword_allowlist: string[]
  keyword_policy_version: string
  context_policy_version: string
  evidence_policy_version: string
  candidate_asset: string
  candidate_enabled: boolean
  candidate_layer1_count: number
  candidate_layer2_count: number
  candidate_source_commit: string
  candidate_endpoints: ContentModerationEndpoint[]
  cyber_policy_exclude_from_ban_count: boolean
}

export type ContentModerationAPIKeyStatusValue = 'unknown' | 'ok' | 'error' | 'frozen'

export interface ContentModerationAPIKeyStatus {
  index: number
  key_hash: string
  masked: string
  status: ContentModerationAPIKeyStatusValue
  failure_count: number
  success_count: number
  last_error: string
  last_checked_at?: string
  frozen_until?: string
  last_latency_ms: number
  last_http_status: number
  last_tested: boolean
  configured: boolean
}

export interface TestContentModerationAPIKeysPayload {
  api_keys?: string[]
  base_url?: string
  model?: string
  timeout_ms?: number
  // null/undefined 沿用已保存配置的代理；0 强制直连；>0 指定代理
  proxy_id?: number
  prompt?: string
  images?: string[]
}

export interface TestContentModerationAPIKeysResponse {
  items: ContentModerationAPIKeyStatus[]
  audit_result?: ContentModerationTestAuditResult
  image_count: number
}

export interface ContentModerationTestAuditResult {
  flagged: boolean
  highest_category: string
  highest_score: number
  composite_score: number
  category_scores: Record<string, number>
  thresholds: Record<string, number>
}

export interface UpdateContentModerationConfig {
  enabled?: boolean
  mode?: ModerationMode
  base_url?: string
  model?: string
  // undefined 不修改；0 清除（直连）；>0 指定代理
  proxy_id?: number
  api_key?: string
  api_keys?: string[]
  api_keys_mode?: 'append' | 'replace'
  delete_api_key_hashes?: string[]
  clear_api_key?: boolean
  timeout_ms?: number
  sample_rate?: number
  all_groups?: boolean
  group_ids?: number[]
  user_email_whitelist?: string[]
  record_non_hits?: boolean
  thresholds?: Record<string, number>
  block_status?: number
  block_message?: string
  email_on_hit?: boolean
  auto_ban_enabled?: boolean
  ban_threshold?: number
  violation_window_hours?: number
  retry_count?: number
  hit_retention_days?: number
  non_hit_retention_days?: number
  pre_hash_check_enabled?: boolean
  blocked_keywords?: string[]
  keyword_blocking_mode?: KeywordBlockingMode
  model_filter?: ContentModerationModelFilter
  cache_version?: string
  cache_max_entries?: number
  cache_max_bytes?: number
  fragment_block_ttl_seconds?: number
  fragment_allow_ttl_seconds?: number
  fragment_ttl_policy_version?: string
  second_layer_enabled?: boolean
  second_layer_stage?: ContentModerationSecondLayerStage
  second_layer_endpoints?: Array<Omit<ContentModerationEndpoint, 'token_configured' | 'token_masked'> & { token?: string }>
  second_layer_scanners?: string[]
  hard_block_patterns?: string[]
  candidate_keywords?: string[]
  keyword_allowlist?: string[]
  keyword_policy_version?: string
  context_policy_version?: string
  evidence_policy_version?: string
  candidate_asset?: string
  candidate_enabled?: boolean
  cyber_policy_exclude_from_ban_count?: boolean
}

export interface ContentModerationArchiveRuntimeStatus {
  degraded: boolean
  retry_queue_depth: number
  emergency_queue_depth: number
  archive_retry_attempts: number
  archive_retry_errors: number
  content_lost: number
  disk_free_bytes: number
  disposition_queue_depth: number
  disposition_retry_attempts: number
  disposition_retry_errors: number
  lost_summary_queue_depth: number
}

export interface ContentModerationBodySizeBucket {
  upper_bound_bytes: number
  count: number
}

export interface ContentModerationRuntimeStatus {
  enabled: boolean
  risk_control_enabled: boolean
  mode: ModerationMode
  pre_block_active: number
  pre_block_checked: number
  pre_block_allowed: number
  pre_block_blocked: number
  pre_block_errors: number
  pre_block_avg_latency_ms: number
  pre_block_api_key_active: number
  pre_block_api_key_available_count: number
  pre_block_api_key_total_calls: number
  pre_block_api_key_loads: ContentModerationAPIKeyLoad[]
  api_key_statuses: ContentModerationAPIKeyStatus[]
  flagged_hash_count: number
  last_cleanup_at?: string
  last_cleanup_deleted_hit: number
  last_cleanup_deleted_non_hit: number
  pending_body_bytes: number
  pending_body_max_seen: number
  pending_body_budget_bytes: number
  pending_body_rejections: number
  observed_request_body_max: number
  request_body_histogram: ContentModerationBodySizeBucket[]
  fragment_cache_hits: number
  fragment_cache_misses: number
  fragment_cache_expired: number
  fragment_cache_replays: number
  fragment_cache_errors: number
  fragment_cache_writes: number
  fragment_cache_write_errors: number
  second_layer_metrics: ContentModerationSecondLayerMetric[]
  archive_runtime: ContentModerationArchiveRuntimeStatus
}

export interface ContentModerationSecondLayerMetric {
  endpoint_id: string
  profile: string
  context_class: string
  evidence_mode: string
  keyword_tier: string
  requests: number
  safe: number
  blocked: number
  uncertain: number
  parser_failures: number
  timeouts: number
  avg_latency_ms: number
}

export interface ContentModerationAPIKeyLoad {
  index: number
  key_hash: string
  masked: string
  status: ContentModerationAPIKeyStatusValue
  active: number
  total: number
  success: number
  errors: number
  avg_latency_ms: number
  last_latency_ms: number
  last_http_status: number
}

export interface ContentModerationLog {
  id: number
  request_id: string
  user_id: number | null
  user_email: string
  api_key_id: number | null
  api_key_name: string
  group_id: number | null
  group_name: string
  endpoint: string
  provider: string
  model: string
  mode: string
  action: string
  cache_hit: boolean
  decision_source: string
  source_log_id?: number
  replay_of_input_hash?: string
  fragment_role?: string
  fragment_kind?: string
  context_class?: string
  fragment_path?: string
  cache_namespace?: string
  policy_version?: string
  model_profile?: string
  prompt_version?: string
  evidence_policy_version?: string
  keyword_tier?: string
  keyword_rule_id?: string
  evidence_mode?: string
  evidence_truncated: boolean
  parser_status?: string
  flagged: boolean
  highest_category: string
  highest_score: number
  matched_keyword: string
  category_scores: Record<string, number>
  threshold_snapshot: Record<string, number>
  input_excerpt: string
  upstream_latency_ms: number | null
  error: string
  violation_count: number
  auto_banned: boolean
  email_sent: boolean
  user_status: string
  queue_delay_ms: number | null
  protocol: string
  transport: string
  request_stage: string
  request_target: string
  input_hash: string
  archive_id?: string
  archive_version?: number
  archive_key_id?: string
  archive_bytes: number
  archive_status: string
  archive_incomplete: boolean
  archive_content_lost: boolean
  archive_deleted_at?: string
  disposition_status: string
  disposition_target: string
  disposition_transitioned: boolean
  legacy_source_job_id?: number
  created_at: string
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

export type ContentModerationLogResult =
  | 'blocked'
  | 'hit'
  | 'pass'
  | 'error'
  | 'cyber_policy'
  | 'content_blocked'
  | 'risky_shadow'

export type ContentModerationLogView = 'cyber_policy' | 'content_blocked' | 'risky_shadow'

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

export async function updateConfig(
  payload: UpdateContentModerationConfig
): Promise<ContentModerationConfig> {
  const { data } = await apiClient.put<ContentModerationConfig>('/admin/risk-control/config', payload)
  return data
}

export async function getStatus(): Promise<ContentModerationRuntimeStatus> {
  const { data } = await apiClient.get<ContentModerationRuntimeStatus>('/admin/risk-control/status')
  return data
}

export async function testAPIKeys(
  payload: TestContentModerationAPIKeysPayload = {}
): Promise<TestContentModerationAPIKeysResponse> {
  const { data } = await apiClient.post<TestContentModerationAPIKeysResponse>('/admin/risk-control/api-keys/test', payload)
  return data
}

export async function listLogs(
  params: ListContentModerationLogsParams = {}
): Promise<ContentModerationLogsResponse> {
  const { data } = await apiClient.get<ContentModerationLogsResponse>('/admin/risk-control/logs', {
    params,
  })
  return data
}

export async function previewArchive(logID: number): Promise<ContentModerationArchivePreview> {
  const { data } = await apiClient.get<ContentModerationArchivePreview>(
    `/admin/risk-control/logs/${logID}/archive/preview`
  )
  return data
}

export async function downloadArchive(logID: number): Promise<Blob> {
  const { data } = await apiClient.get<Blob>(
    `/admin/risk-control/logs/${logID}/archive/download`,
    { responseType: 'blob' }
  )
  return data
}

export async function deleteArchive(logID: number): Promise<DeleteContentModerationArchiveResponse> {
  const { data } = await apiClient.delete<DeleteContentModerationArchiveResponse>(
    `/admin/risk-control/logs/${logID}/archive`
  )
  return data
}

export async function unbanUser(userID: number): Promise<ContentModerationUnbanUserResponse> {
  const { data } = await apiClient.post<ContentModerationUnbanUserResponse>(
    `/admin/risk-control/users/${userID}/unban`
  )
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
  testAPIKeys,
  listLogs,
  previewArchive,
  downloadArchive,
  deleteArchive,
  unbanUser,
  deleteFlaggedHash,
  clearFlaggedHashes,
}

export default riskControlAPI
