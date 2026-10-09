// Mirrors the JSON shapes of pkg/server and pkg/agent.

export type RunPhase =
  | 'planning'
  | 'plan_review'
  | 'running'
  | 'leaf_approval'
  | 'ask'
  | 'paused'
  | 'done'
  | 'failed'

export type ApprovalMode = 'auto' | 'manual'

export interface PhaseSnapshot {
  phase: RunPhase
  pending_ask?: string
  leaf_approving?: string
  approval_mode: ApprovalMode
  session_id: string
  workdir: string
  error?: string
}

export interface SessionParams {
  goal: string
  workdir?: string
  model?: string
  base_url?: string
  api_key?: string
  parallel?: number
  isolate?: boolean
  git_commit?: boolean
  web?: boolean
  browser?: boolean
  approval_mode?: ApprovalMode
  budget_tokens?: number
  max_cost?: number
}

export interface SessionRow {
  id: string
  goal: string
  updated_at: string
  root_state: string
  leaves_done: number
  leaves_all: number
  live?: PhaseSnapshot
}

export interface SessionDetail {
  session_id: string
  workdir: string
  live?: PhaseSnapshot
  // The raw tree JSON; Task 8 introduces a typed view.
  tree: unknown
}

export interface CreateResult {
  session_id: string
  workdir: string
  phase: PhaseSnapshot
}

// SSE / JSONL events. Every event may carry a seq (monotonic, persisted).
export interface StreamEvent {
  seq?: number
  ts?: string
  kind: string
  node_id?: string
  [key: string]: unknown
}

// FileChange mirrors pkg/tools.FileChange (leaf diff review payload).
export type FileChangeStatus = 'added' | 'modified' | 'deleted'

export interface FileChange {
  path: string
  status: FileChangeStatus
  conflict?: boolean
  binary?: boolean
  truncated?: boolean
  old_mode?: number
  new_mode?: number
  old_content?: string
  new_content?: string
}

export interface ApprovalPayload {
  session_id: string
  node_id: string
  title: string
  changes: FileChange[]
}

// ---- Task tree (mirrors pkg/models + pkg/engine.TaskTree JSON) ----

export type NodeState =
  | 'PENDING'
  | 'DECOMPOSING'
  | 'RUNNING'
  | 'VERIFYING'
  | 'COMPLETED'
  | 'FAILED'
  | 'SKIPPED'

export type NodeType = 'COMPOUND' | 'LEAF'

export interface TokenUsage {
  calls?: number
  prompt_tokens?: number
  completion_tokens?: number
  total_tokens?: number
}

export interface ErrorRecord {
  time: string
  error: string
  fingerprint?: string
}

export interface ContractSpec {
  inputs?: string[]
  outputs?: string[]
  dependencies?: string[]
  constraints?: string[]
}

export interface DoD {
  description?: string
  commands?: string[]
  expected_output?: string
  timeout_sec?: number
}

export interface TaskNode {
  id: string
  parent_id?: string
  title: string
  description: string
  type: NodeType
  state: NodeState
  depth: number
  children_ids?: string[]
  contract: ContractSpec
  dod: DoD
  retry_count?: number
  max_retries?: number
  decompose_count?: number
  error_msg?: string
  error_history?: ErrorRecord[]
  result_summary?: string
  token_usage: TokenUsage
  integration_verified?: boolean
  created_at: string
  updated_at: string
}

export interface TaskTree {
  id: string
  goal?: string
  work_dir?: string
  root_id: string
  nodes: Record<string, TaskNode>
  created_at: string
  updated_at: string
  model_name?: string
}

// ---- Read-only workspace file browser (GET /api/fs/*) ----

export type FsEntryType = 'dir' | 'file' | 'symlink' | 'other'

export interface FsEntry {
  name: string
  path: string
  type: FsEntryType
  size: number
  mode: number
  escaped?: boolean
}

export interface FsListResponse {
  path: string
  entries: FsEntry[]
  truncated: boolean
}

export interface FsFileResponse {
  path: string
  size: number
  mode: number
  content: string
}

export interface LogPage {
  lines: string[]
  offset: number
  next_offset: number
  total: number
  has_more: boolean
}

