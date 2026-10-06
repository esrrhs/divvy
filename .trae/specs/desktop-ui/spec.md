# Divvy 桌面前端（Tauri + React）- 产品需求文档

## Overview
- **Summary**: 为 divvy 增加一个 Cursor 风格的图形界面：新增 `divvy serve` 本地服务（HTTP + SSE，Go 实现并 go:embed 托管前端产物），React/Vite 前端提供对话式发起、实时任务树、叶子 diff 审查与审批、会话管理/续跑、工作区文件浏览；再以 Tauri sidecar 模式打包为桌面应用。
- **Purpose**: CLI 对普通用户不友好；任务树、验收失败、diff、会话续跑这些核心能力目前只有滚动文本，难以定位和操作。图形界面把「监督弱模型干活」变成可视化、可干预的流程。
- **Target Users**: 使用 divvy 驱动本地/弱模型完成编程任务的开发者，尤其是需要频繁查看进度、审批改动、中断续跑的用户。

## Goals
- 浏览器（`divvy serve`）与 Tauri 桌面应用两种方式打开同一套界面。
- 发起目标后，实时展示拆解过程、叶子状态流转、验收/重试、token 与成本。
- 在界面上完成 guided 全流程：plan 审批/调整/中止、叶子改动的 diff 审查（批准/拒绝重做）、叶子 ask 问答、暂停/续跑、/add、/redo。
- 历史会话列表、查看任务树与事件流、一键 resume。
- 侧边栏只读浏览工作区文件并查看内容。

## Non-Goals
- 不做 IDE 级代码编辑器（无 Monaco/代码补全/AI inline edit）；文件浏览本期只读。
- 不做多用户、远程访问、鉴权账号体系；仅本机单用户。
- 不做桌面应用的代码签名/公证与自动更新；Tauri 本期只产出可开发运行与未签名构建。
- 不改变批处理 CLI（`divvy "goal"`）、REPL、guided 终端模式的既有行为。
- 不做移动端适配。

## Background & Context
- 已有可复用资产：
  - `agent.GuidedIO` 是通道化接口（`pkg/agent/guided.go`），plan 审批、ask、/add、/redo、pause 全部经它交互；Web 端提供新实现即可复用 Guider 生命周期。
  - `EventRecorder` 写 JSONL 事件流（state_change/llm_call/tool_call/verify/retry/finish_gate 等，`pkg/agent/events.go`），是实时事件的唯一汇聚点。
  - `engine.TaskTree.ToJSON()` 可直接序列化整棵树；`Storage.ListSessions/LoadTree` 已支持会话枚举与恢复；LATEST 指针支持裸 resume。
  - `Scheduler.OnStateChange` 已有状态变更回调。
  - Logger 已有 Tee 机制可导出日志行。
- 差距：
  - guided 目前只在 **plan 级**审批；**叶子 finish 后、MergeBack 前没有人工 diff 审批点**，需要在 `executeLeaf` 新增可配置 ApprovalHook（批处理/终端 guided 默认不启用，保持旧行为）。
  - `EventRecorder` 只有文件 sink，没有进程内订阅 fan-out。
  - 仓库当前无任何 HTTP/前端代码；CI 只有 Go/Chrome 两类 job。
- 架构约束：项目哲学是依赖极少、单二进制。前端构建链（Node）只应出现在 web/ 目录与 CI，Go 主构建在无 Node 环境下仍须成功（go:embed 内置占位资源，真实产物由 CI/发布构建注入）。

## Functional Requirements

- **FR-1 服务与托管**：`divvy serve` 启动 HTTP 服务，仅绑定 127.0.0.1，端口可指定（默认 0 = 系统分配），启动时生成随机 URL token，启动行输出实际访问 URL；静态资源由 go:embed 提供（无预构建产物时返回提示页，引导开发模式）。
- **FR-2 会话启动**：POST 新建会话，提交 goal 与运行参数（model/baseURL/apiKey 来源说明、parallel/isolate/git-commit/web/browser、预算等），后端用 Guider 生命周期异步运行；立即返回 session id。
- **FR-3 实时流**：每个会话一条 SSE，推送事件流（JSONL 同款事件）、任务树快照（状态变更/检查点后）、日志行；前端断线重连后可用「最近 N 条事件 + 当前树」补齐状态。
- **FR-4 任务树可视化**：三栏布局中的主区以树形展示节点（COMPOUND/LEAF、状态色、重试次数、耗时、token/成本、错误摘要），节点点击展示详情（契约、DoD、错误历史、结果摘要）。
- **FR-5 Plan 审批**：计划就绪时界面弹出审批条，支持「批准执行」「填写调整意见」「中止」，语义与终端 guided 的 /approve、调整、/abort 完全一致；中止时计划已保存但不执行。
- **FR-6 叶子 Diff 审批（新机制）**：提供审批模式 `auto`（不中断，默认）、`manual`（每个叶子 finish 后、MergeBack 前暂停，展示本次变更文件的并排/统一 diff，批准才合并发布，拒绝则附带意见重置该叶子重做）。auto 与现有所有模式行为一致。
- **FR-7 Ask 问答**：叶子调用 ask 时界面弹出问题输入框，回答回传 worker；支持以"暂停"回答结束运行。
- **FR-8 运行控制**：界面提供暂停（保存树后停止，等价 /pause）、redo 指定节点、add 新叶子指令、resume 指定会话。
- **FR-9 会话管理**：列出 DataDir 下全部会话（goal、更新时间、根状态、叶子进度），支持打开查看（树+事件流+日志），支持对未完成会话 resume；删除会话复用现有 prune 能力（本期至少支持单会话删除的后端 API，前端可后补）。
- **FR-10 文件浏览**：侧边栏按 workdir 根列出目录树（套用 skipDirNames 忽略规则），点击查看文本文件内容；所有路径解析必须被沙箱约束在 workdir 内，拒绝 `..`、绝对路径、符号链接逃逸。
- **FR-11 Tauri 外壳**：Tauri 应用启动时以 sidecar 拉起本机 `divvy serve`（或开发态直连 vite + go serve），WebView 载入界面，退出应用时结束 sidecar；先支持 macOS 开发运行。
- **FR-12 配置来源**：serve 自身参数走 flag；LLM 配置默认沿用环境变量（OPENAI_BASE_URL/API_KEY/MODEL），新建会话时可在界面覆盖（apiKey 仅保存在内存/本机，不落事件流与日志）。

## Non-Functional Requirements

- **NFR-1 安全**：服务仅监听 loopback；除静态资源外所有 API 必须校验启动 token（header 或 query）；文件 API 与命令运行一律限制在 workdir；apiKey 不写入 events/log，前端不在事件流中回显。
- **NFR-2 单二进制友好**：`go build ./...` 与 `go test ./...` 在仅有 Go 工具链的环境必须通过；前端产物缺失不阻断 Go 编译与后端测试。
- **NFR-3 向后兼容**：不新增 flag 时，所有现有 CLI 模式行为不变；FR-6 的审批 hook 默认关闭。
- **NFR-4 可测试**：server 包 HTTP/SSE/审批流程用 httptest + ScriptedClient 风格测试；ApprovalHook 有独立单元测试（暂停→批准合并 / 拒绝重做）。
- **NFR-5 体验**：常规操作（状态变更、日志）端到端延迟 < 1s（本机）；界面在 200 节点树、1 万事件流下仍可流畅浏览（虚拟列表或截断加载）。
- **NFR-6 并发安全**：同时只允许一个会话处于 running（对同一 workdir 的第二启动请求返回 409）；SSE 多订阅者 fan-out 不阻塞编排主循环。
- **NFR-7 可移植**：Go 侧仅用标准库 net/http + 已有依赖，不引入 Web 框架；前端依赖锁定在 web/ 子目录。

## Constraints
- **Technical**: Go 1.27（当前工具链）；React 18 + Vite + TypeScript + Tailwind；SSE 而非 WebSocket（单向事件流 + POST 指令即可满足）；Tauri v2 sidecar；macOS arm64 为首期平台。
- **Business**: 本期不做签名分发；界面语言以中文为主、英文术语保留。
- **Dependencies**: 新增 Node 构建仅用于 web/；Go 侧不新增第三方依赖（SSE/HTTP 用标准库实现）。

## Assumptions
- 用户在本机运行，浏览器/Tauri 与 divvy serve 同机；loopback + token 的威胁模型足以应对本机网页 CSRF（DNS rebinding 防护：Host 头校验仅允许 localhost/127.0.0.1）。
- 叶子 diff 可由 worktree 镜像的 git 能力直接产出（git 仓库：`git diff` + 未跟踪文件；非 git：文件级增删改清单，不展示行级 diff）。
- Tauri sidecar 二进制在开发机由 `go build` 产出；打包产物形态（是否提交 dist）在 Plan 阶段确定，默认提交 web/dist 以保持单仓库可复现构建。

## Acceptance Criteria

### AC-1: `divvy serve` 可用且仅本机可访问
- **Type**: `rule`
- **Given**: 已构建 divvy 二进制
- **When**: 执行 `divvy serve -workdir <dir>`
- **Then**: 进程输出含 token 的 loopback URL；无 token 请求 /api/sessions 返回 401；服务只绑定 127.0.0.1（netstat/lsof 可见）；GET 根路径返回前端页面（或无构建产物时的开发提示页）
- **Pass Condition**: httptest/进程级测试覆盖 token 校验与 Host 校验，手工验证 URL 可打开
- **Evidence**: `pkg/server` 测试输出；启动日志截图或文本

### AC-2: 发起目标并实时看到任务树流转
- **Type**: `rule`
- **Given**: serve 运行中，ScriptedClient（测试）或真实模型
- **When**: POST /api/sessions 提交 goal 并连接该会话 SSE
- **Then**: 依次收到 plan 就绪、节点 DECOMPOSING/RUNNING/VERIFYING/COMPLETED 等事件与至少一次树快照；前端树视图状态与事件一致；断线重连后状态不丢
- **Pass Condition**: 后端测试断言 SSE 收到 state_change 与 tree_snapshot；界面手工演示一次完整运行
- **Evidence**: server 测试日志；界面录屏/截图

### AC-3: 界面完成 plan 审批三动作
- **Type**: `rule`
- **Given**: 计划已生成等待审批
- **When**: 分别调用 approve / adjust(带意见) / abort
- **Then**: approve 后开始执行；adjust 触发 replan 并推送新树；abort 后计划保存但无叶子进入 RUNNING，会话停止
- **Pass Condition**: 三条路径各有后端测试（复用 ScriptedClient）
- **Evidence**: `pkg/server` 审批相关测试

### AC-4: 叶子 diff 审批：批准合并 / 拒绝重做
- **Type**: `rule`
- **Given**: 审批模式 manual，叶子 finish 且 verify 通过、尚未 MergeBack
- **When**: 界面获取该叶子 diff（含变更文件与内容），点击批准
- **Then**: MergeBack 发生、文件进入 workdir、叶子 COMPLETED；当点击拒绝并填写意见时，叶子被重置 PENDING 并带着意见重新执行，原镜像改动不进入 workdir
- **Pass Condition**: ApprovalHook 单元测试覆盖两分支 + auto 模式零中断的回归测试
- **Evidence**: `pkg/agent` 审批 hook 测试、server 集成测试

### AC-5: 历史会话可列举、查看与续跑
- **Type**: `rule`
- **Given**: DataDir 中有若干历史会话（含未完成会话）
- **When**: GET 会话列表、打开某会话、对未完成会话点 resume
- **Then**: 列表字段与 Storage.ListSessions 一致；详情返回树 JSON 与事件 JSONL；resume 后编排继续推进
- **Pass Condition**: API 测试 + 手工点击验证
- **Evidence**: server 测试；界面截图

### AC-6: 文件浏览被约束在 workdir 内
- **Type**: `rule`
- **Given**: serve 以某 workdir 启动
- **When**: 请求 `../etc/passwd`、绝对路径、指向目录外的符号链接
- **Then**: 一律 400/403；正常目录列举（带忽略规则）与文本文件读取 200
- **Pass Condition**: 路径穿越测试用例全部通过
- **Evidence**: server 文件 API 测试

### AC-7: ask 问答可达
- **Type**: `rule`
- **Given**: 叶子 worker 调用 ask
- **When**: 界面提交回答
- **Then**: worker 收到回答并继续；SSE 可观察到 ask_pending/ask_resolved 事件
- **Pass Condition**: 一条端到端测试覆盖提问→回答
- **Evidence**: server 集成测试

### AC-8: 界面可用性达到 Cursor 风格水准
- **Type**: `rubric`
- **Dimension**: 界面信息架构与交互质量（三栏布局、状态一目了然、审批/对话操作顺畅、中文文案准确）
- **Scale**: 1-5
- **Anchors**: 1 = 仅接口演示无可视化；3 = 功能齐全但布局拥挤、需频繁刷新；5 = 三栏清晰、实时更新、diff 与审批流畅，接近 Cursor 观感
- **Pass Threshold**: >= 4
- **Evidence**: 最终桌面/浏览器界面截图与操作走查

### AC-9: Tauri 外壳在 macOS 可启动
- **Type**: `rule`
- **Given**: macOS + 已 go build 出 divvy 二进制 + Node 依赖已安装
- **When**: 在 app/ 目录执行 tauri dev
- **Then**: 窗口打开并显示界面，后台 sidecar 为 divvy serve；关闭窗口时 sidecar 进程退出
- **Pass Condition**: 手工走查通过；文档写明前置条件
- **Evidence**: 运行记录与截图

### AC-10: 无 Node 环境 Go 全量构建测试不中断
- **Type**: `rule`
- **Given**: 仅有 Go 工具链的干净环境
- **When**: `go build ./... && go test ./...`
- **Then**: 全部通过（embed 占位资源兜底）；CI 的 Go job 无需 Node
- **Pass Condition**: 现有 CI 四个 job 保持绿色（允许新增独立 web job）
- **Evidence**: CI 运行结果

### AC-11: 机密不落地
- **Type**: `rule`
- **Given**: 新建会话时在界面填入自定义 apiKey
- **When**: 运行结束后检查 events/*.jsonl、logs/*.log 与磁盘上会话数据
- **Then**: 无 apiKey 明文；内存外仅存在于向 LLM 发出的请求
- **Pass Condition**: 代码走查 + 一条断言事件流不含密钥的测试
- **Evidence**: server 测试与 grep 检查

## Open Questions
- [ ] Q1: manual 审批模式是否也对"仅警告（debug print/大 diff）"暂停？建议只对 blocking gate 与 manual 叶子暂停，警告仅展示。→ Plan 中按此默认实现，用户可在试用后调整。
- [ ] Q2: web/dist 是否提交进仓库？倾向提交（单仓库可复现、go:embed 最简单），代价是前端改动产生构建产物 diff；CI web job 负责校验 dist 与源码一致。
- [ ] Q3: 本期 Tauri 是否产出 Windows/Linux 构建？建议否，仅 macOS dev；矩阵扩展下期。
