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

export type AgentMode = 'guided' | 'direct'

export interface PhaseSnapshot {
  phase: RunPhase
  pending_ask?: string
  leaf_approving?: string
  approval_mode: ApprovalMode
  session_id: string
  workdir: string
  error?: string
  agent_loop?: AgentMode | string
  pending_inject?: string
  in_tool?: boolean
  summary?: string
  live_assistant?: string
  live_stream_id?: string
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
  agent_mode?: AgentMode
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
  // Server-side replay tag: frames replayed from the JSONL tail on
  // (re)connect are history — they populate the timeline but must not
  // drive the live phase machine or replace the current tree.
  replay?: boolean
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

export interface DiffHunkLine {
  type: 'context' | 'add' | 'remove' | string
  old_no?: number | null
  new_no?: number | null
  text: string
}

export interface ReviewHunk {
  index: number
  lines: DiffHunkLine[]
}

export interface HunkDecision {
  index: number
  accept: boolean
}

export interface ApplyHunkRequest {
  path: string
  all?: 'accept' | 'reject' | string
  decisions?: HunkDecision[]
}

/** A persisted turn (or leaf-approval) file change, optionally with server hunks. */
export interface TurnChange extends FileChange {
  hunks?: ReviewHunk[]
  hunk_count?: number
  editable?: boolean
  edit_block_reason?: string
  hunk_decisions?: string[]
  applied_content?: string
  applied_status?: string
}

export interface TurnChangesResponse {
  session_id: string
  changes: TurnChange[]
  editable?: boolean
  reason?: string
}

export interface ApprovalPayload {
  session_id: string
  node_id: string
  title: string
  changes: TurnChange[]
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

// ---- Workspace file browser and text editor (GET/PUT /api/fs/*) ----

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
  hash?: string
  editable?: boolean
  redacted?: boolean
  reason?: string
}

export interface FsWriteResponse {
  path: string
  size: number
  hash: string
  editable?: boolean
}

export interface ProjectSearchHit {
  path: string
  line: number
  preview: string
}

export interface ProjectSearchResponse {
  query: string
  literal: boolean
  regex: boolean
  case_sensitive: boolean
  glob?: string
  hits: ProjectSearchHit[]
  truncated: boolean
  scanned: number
}

export interface CodeSelection {
  path: string
  start_line: number
  end_line: number
  text: string
  draft?: boolean
}

export interface LogPage {
  lines: string[]
  offset: number
  next_offset: number
  total: number
  has_more: boolean
}



// ---- Project conversations (persistent thread per workdir) ----

export interface ConversationSettings {
  model?: string
  base_url?: string
  parallel?: number
  isolate: boolean
  git_commit: boolean
  web: boolean
  browser: boolean
  approval_mode?: ApprovalMode
  budget_tokens?: number
  max_cost?: number
  agent_mode?: AgentMode
}

export type ConvMessageKind = 'user' | 'run_started' | 'run_finished' | 'notice' | 'need_key' | 'model_reply' | 'tool_activity'

export interface FileRef {
  path: string
  type: 'file' | 'dir' | 'selection' | string
  bytes?: number
  excluded?: string
  truncated?: boolean
  start_line?: number
  end_line?: number
  draft?: boolean
}

export interface QueuedTurn {
  text: string
  paths?: string[]
  selections?: CodeSelection[]
  created_at: string
}

export interface PendingSteer {
  text: string
  paths?: string[]
  selections?: CodeSelection[]
  created_at: string
}

export interface TurnChangeSummary {
  path: string
  status: FileChangeStatus | string
  binary?: boolean
  truncated?: boolean
}

export type MessageMode = 'start' | 'queue' | 'takeover' | 'steer'

export interface ConvMessage {
  id: string
  role: 'user' | 'system' | 'assistant'
  kind: ConvMessageKind | string
  text: string
  session_id?: string
  outcome?: string
  created_at: string
  attachments?: FileRef[]
  changes?: TurnChangeSummary[]
}

export interface PendingRollback {
  session_id?: string
  phase?: string
  error?: string
  files?: string[]
  applied?: string[]
  conflict_paths?: string[]
  message?: string
}

export interface Conversation {
  id: string
  title: string
  workdir: string
  created_at: string
  updated_at: string
  settings: ConversationSettings
  messages: ConvMessage[]
  work_log: string[]
  session_ids: string[]
  current_session_id?: string
  queue?: QueuedTurn
  steer?: PendingSteer
  rolled_back_turns?: string[]
  pending_rollback?: PendingRollback
  has_api_key: boolean
  context_note: string
  run_phase?: RunPhase | string
  run_session_id?: string
  key_required?: boolean
  agent_loop?: AgentMode | string
  pending_inject?: string
  inject_state?: string
  in_tool?: boolean
  pending_ask?: string
  live_assistant?: string
  live_stream_id?: string
  leaf_approving?: string
}

// ---- AI inline edit (Cursor Cmd-K style) ----

/** The line range the user highlighted. `insert_line` makes it an insertion. */
export interface AiEditSelection {
  start_line?: number
  end_line?: number
  insert_line?: number
}

/** One earlier round of an inline edit: the instruction and what came back. */
export interface AiEditTurn {
  instruction: string
  code: string
}

export interface AiEditRequest {
  path: string
  selection: AiEditSelection
  instruction: string
  stream?: boolean
  /** Earlier turns, so a follow-up revises the previous proposal. */
  history?: AiEditTurn[]
}

/** The server diffs the model's code reply against the region it sent. */
export interface AiEditResponse {
  path: string
  base_hash: string
  start_line: number
  end_line: number
  insert_line?: number
  old_text: string
  new_text: string
  hunks: ReviewHunk[]
  preview: string
  truncated?: boolean
}

export interface AiEditApplyRequest {
  path: string
  base_hash: string
  start_line: number
  end_line: number
  insert_line?: number
  old_text: string
  new_text: string
  decisions: boolean[]
}

export interface AiEditApplyResponse {
  path: string
  applied: number
  rejected: number
  new_content: string
  conflict: boolean
  conflict_path?: string
}

export interface AiEditUndoResponse {
  path: string
  restored: boolean
  new_content: string
  /** Writes that can still be undone after this one. */
  remaining?: number
}
