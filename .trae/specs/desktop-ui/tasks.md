# Divvy 桌面前端（Tauri + React）- 实施计划

切片原则：后端先行、接口可独立测试；前端按页面能力纵向拆；Tauri 最后整合。每个切片保持仓库可编译、CLI 行为不变。

## Task 1: 事件订阅总线与树快照推送
- **Status**: `completed`
- **Priority**: high
- **Depends On**: None
- **Completion Evidence**:
  - `pkg/agent/events.go`：EventRecorder 新增 Subscribe/SubscriberCount，fan-out 非阻塞丢弃 + `_dropped` 通知，Close 注销全部订阅者。
  - `pkg/agent/treebroadcast.go`（新）：100ms 去抖的 tree_snapshot 广播；state 变更走去抖、checkpoint/session_end 同步发布；tree 以 json.RawMessage 内嵌。
  - `pkg/agent/logger.go`：SubscribeLines 行订阅（SSE 日志面板来源）。
  - `pkg/agent/eventsbus_test.go`（新）7 个测试全绿；`go test ./pkg/agent/ ./pkg/engine/ ./pkg/tools/` 全部通过；TR-1.1/TR-1.2 满足。
- **Description**:
  - `EventRecorder` 增加进程内 fan-out：`Subscribe() (ch, cancel)`，Record 在写文件之外把同一 map 广播给订阅者（缓冲、非阻塞丢弃策略 + 计数字段标记丢弃）。
  - 确认 Logger Tee 能力（已存在则直接复用，没有则补一个同样式 fan-out）。
  - 在编排层增加「树变更即推送」钩子：state 变更/checkpoint/decompose 后发 `tree_snapshot` 事件（载荷为 ToJSON 后的树），server 层订阅即可，不耦合 HTTP。
- **Acceptance Criteria Addressed**: AC-2, AC-10
- **Test Requirements**:
  - `rule` TR-1.1: 两个订阅者都能收到后续全部事件；订阅取消后不再收到且不泄漏 goroutine；慢消费者不阻塞 Record（带丢弃计数）。证据：`pkg/agent` 新增测试。
  - `rule` TR-1.2: 叶子状态迁移时 SSE 数据口径里出现 tree_snapshot。证据：hook 单测。
- **Notes**: tree_snapshot 可能较大，钩子内 ToJSON 放异步/限频（同节点 100ms 合并一次）。

## Task 2: 叶子级人工审批钩子（ApprovalHook）
- **Status**: `completed`
- **Priority**: high
- **Depends On**: None
- **Completion Evidence**:
  - `pkg/agent/config.go`：`LeafApproval`（auto/manual）+ `LeafApprovalMode()` 归一化；manual 不允许无 isolate（`New` 构造期报错 "manual leaf approval requires isolate mode"）。
  - `pkg/agent/approval.go`（新）：`LeafApprovalRequest{NodeID,Title,Changes}` / `LeafApprovalDecision{Approved,Comment}` / `LeafApprovalHook`；`Orchestrator.SetLeafApprovalHook`。
  - `pkg/agent/orchestrator.go`：verify 通过后、publishLeaf 前挂起；批准 → 继续 MergeBack；拒绝 → recordNodeError（RetryCount++、意见进 ErrorHistory）→ 关闭旧 mirror → `newLeafMirror()` 重新快照 → `retryOrGiveUp` 走既有退避/预算/停滞闸门 → 重跑；leaf_approval 事件（pending/approved/rejected）；ctx 取消可中断挂起并 checkpoint；未装 hook 时 warn 并降级 auto。
  - `pkg/tools/mirror.go`：MergeBack 拆出无副作用的 `classifyChanges`；新增 `PreviewChanges() []FileChange`（added/modified/deleted + Old/NewContent + Old/NewMode + 二进制 NUL 启发 + 单文件 256KB/总量 4MB 截断 + conflict 标记），git worktree 与非 git copy 共用同一实现；MergeBack 现保留 mirror 内权限位（chmod +x 不丢）；`Close` 用 sync.Once 幂等。
  - 实现偏差（对原计划更简）：未分叉调用 `git status/diff`，改为统一文件级快照 diff（含全文与权限位），前端可自行渲染 unified diff；TR-2.4 两种工作区载荷一致。
  - 测试：`pkg/agent/approval_test.go`（新）TR-2.1/2.2/2.3 四测试；`pkg/tools/mirror_test.go` TR-2.4 `TestMirror_PreviewChangesNoApply` table-driven（copy/worktree 两子用例）+ 冲突标记/二进制/可执行位保留；`go test ./... -race`、`go vet ./...`、gofmt 全绿。
- **Description**:
  - Config 增加 `LeafApproval string`（"auto"/"manual"，默认 auto）。
  - `executeLeaf` 中 verify 通过后、`publishLeaf` 前插入 hook：manual 模式下挂起，向外部暴露：节点 id、镜像 worktree 路径、变更文件清单与 diff 内容；返回批准/拒绝+意见。
  - 批准 → 继续 MergeBack/publish；拒绝 → 节点重置 PENDING（或等价重试路径）并把用户意见作为 prevError 注入新一轮；不合并任何镜像改动，镜像关闭。
  - diff 产出：git 仓库（含 worktree 镜像）用 `git status/diff`（含未跟踪文件全文或 unified diff）；非 git 工作区降级为文件级增删改清单。
  - 批处理与终端 guided 默认 auto，行为与今天完全一致。
- **Acceptance Criteria Addressed**: AC-4, AC-10
- **Test Requirements**:
  - `rule` TR-2.1: auto 模式下 executeLeaf 全流程无人工介入完成。证据：现有全部 agent 测试不改通过。
  - `rule` TR-2.2: manual 批准分支：挂起→批准→MergeBack 执行→COMPLETED，workdir 出现新内容。证据：新单测。
  - `rule` TR-2.3: manual 拒绝分支：拒绝+意见→不 MergeBack→节点重跑且新一轮输入含意见；重试后批准可成功。证据：新单测。
  - `rule` TR-2.4: git 与非 git 工作区都能产出变更清单（非 git 至少有文件级清单）。证据：table-driven 测试。
- **Notes**: 与现有 merge-conflict 重拍路径复用 mirror 重建逻辑；并发叶子各自独立审批通道。

## Task 3: Web 版 GuidedIO 与运行管理器
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 1, Task 2
- **Completion Evidence**:
  - `pkg/agent/webguided.go`（新）：`channelGuided`——HTTP POST → channel 的 GuidedIO；32 行缓冲覆盖 decompose/root-acceptance 不消费窗口；Submit 随 run ctx 取消返回；channel 不关闭（web 无 EOF 语义，避免 send-on-closed）。
  - `pkg/agent/guided.go`：review 每轮发 `plan_review phase=pending` 结构化事件；Guider 新增 `askNotice` 钩子，worker ask 等待时通知前端（答案仍走 Lines）。
  - `pkg/agent/runmanager.go`（新）：`RunManager`（active[sessionID] + busy[absWorkdir]，`:starting:` 占位消除并发 Start 竞态）；`RunHandle` 七态状态机（planning/plan_review/running/leaf_approval/ask/paused/done/failed），由事件订阅（plan_review/leaf_approval/state_change/user_pause）+ askNotice 驱动；动作 ApprovePlan/AdjustPlan/Abort/Pause/AddInstruction/Redo/Answer/DecideLeaf，全部相位守卫与哨兵错误（ErrWorkdirBusy/ErrSessionActive/ErrRunFinished/ErrWrongPhase）；manual 模式自动挂 leaf 审批桥（按 NodeID 路由 decision，ctx 取消可中断）；Start/Resume（Load 恢复 workdir 并校验）；run 退出释放 workdir 锁并 Close orchestrator。
  - `pkg/agent/runmanager_test.go`（新）9 测试：TR-3.1 Start→approve→完成全通道驱动；TR-3.2 同 workdir 冲突/异 workdir 并行/结束后锁释放；TR-3.3 adjust/abort/pause/add/redo/answer 逐动作 + 相位守卫；`go test ./... -race`、`go vet ./...`、gofmt 全绿。
- **Description**:
  - 新增 `pkg/server/run`（或 pkg/agent/webguided.go）：通道版 `GuidedIO`（HTTP POST → channel）、askHook 桥（ask 事件 + answer POST 汇合）。
  - `RunManager`：map[sessionID]*runHandle；同一 workdir 同时只允许一个 running（第二个启动/ resume 返回冲突）；封装 start(new goal)/resume/pause/approve/adjust/abort/redo/add/answer 与 SSE 订阅出口；进程退出时 ctx 取消并 checkpoint。
  - 指令幂等与状态保护：非等待审批态的 approve 返回 409 语义错误；已结束会话拒绝执行指令。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-5, AC-7, AC-8
- **Test Requirements**:
  - `rule` TR-3.1: ScriptedClient 下 new→approve→完成全程由通道驱动，无终端参与。证据：跑批式集成测试。
  - `rule` TR-3.2: 同 workdir 并发启动第二次返回冲突；不同 workdir 可并行。证据：单测。
  - `rule` TR-3.3: adjust/abort/redo/add/pause/answer 各自映射到 Guider 既有语义。证据：逐动作测试。

## Task 4: HTTP/SSE 服务骨架与安全中间件
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 3
- **Completion Evidence**:
  - 新包 `pkg/server`（仅标准库）：`server.go` Server/New（crypto/rand 24 字节 URL-safe token）、`Start(port)` 只绑 `127.0.0.1`、返回带 token 的访问 URL 与实际 Addr；中间件链 hostGuard→tokenGuard：Host 白名单 localhost/127.0.0.1/[::1]（含端口形态，其余 403，防 DNS rebinding），`/api/` 校验 Bearer/query token（constant-time 比较，失败 401+WWW-Authenticate），静态资源免 token；`/api/health`。
  - `sse.go`：GET `/api/sessions/{id}/events`；先订阅再补发 → 无缝隙；tail 200 条 JSONL（环形缓冲、单行上限 16MB）+ 订阅后权威 tree_snapshot；实时 fan-out 按 `seq` 去重；支持 `Last-Event-ID` 断点续传（SSE `id:` 即 seq）；15s `: ping` 心跳（可 Option 覆盖）；历史会话（非 live）读盘补发后仅心跳；未知会话 404。
  - `assets.go` + `placeholder/index.html`：go:embed 占位深色提示页，无 Node 可编译；`WithStatic(fs.FS)` 供 Task 11 注入真实 dist；未知路径 SPA 回退 index.html。
  - `redact.go`：Redact/RedactBytes（出站 SSE 按 live handle 的 SecretValues 即内存 apiKey 逐帧掩码）、MaskSecret（启动/状态行）。
  - `pkg/agent/events.go`：EventRecorder 新增单调 `seq`（落盘与 fan-out 同一把锁内赋值，SSE 补发/实时去重与 Last-Event-ID 的基础）；RunHandle 新增 SubscribeEvents、SecretValues 访问器。
  - 测试（11 个，httptest）：TR-4.1 无/错 token 401、Bearer/query 200、恶意 Host 403、loopback 形态 200、占位页免 token、Start 实测 127.0.0.1 绑定与 token URL；TR-4.2 脚本跑批：plan_review 补发 + tree_snapshot + 心跳 → approve 后实时 state_change/tree_snapshot → 断线重连补发恢复 → Last-Event-ID 只收更新事件；TR-4.3 哨兵 apiKey 全 DataDir 落盘扫描 + SSE 帧双重断言无明文；`go test ./... -race`（pkg/server 连跑 3 轮）、vet、gofmt 全绿。
- **Description**:
  - 新增 `pkg/server`：标准库 net/http 路由；启动生成随机 token；loopback 绑定；中间件校验 `Authorization: Bearer`/query token 与 Host 白名单（localhost/127.0.0.1，防 DNS rebinding）。
  - SSE：GET /api/sessions/{id}/events，先补发最近 N(=200) 条事件 + 当前树快照，再挂订阅；心跳 15s。
  - 静态资源 go:embed（Task 9 产物），缺失时内置占位首页。
  - apiKey 仅存内存 runHandle；日志/事件落盘前做密钥掩码（复用现有不记 full transcript 的口径并加针对性断言）。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-11
- **Test Requirements**:
  - `rule` TR-4.1: 无/错 token → 401；Host 非白名单 → 403；正确 token → 200。证据：httptest。
  - `rule` TR-4.2: SSE 建连收到补历史 + 后续实时事件 + 心跳；断线重连状态可恢复。证据：httptest SSE 测试。
  - `rule` TR-4.3: 事件 JSONL、日志文件中 grep 不到测试 apiKey。证据：测试断言。

## Task 5: 业务 API（会话/审批/文件）
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 4
- **Completion Evidence**:
  - `pkg/server/sessions.go`（新）：POST/GET `/api/sessions`（goal+参数→Config 分层覆盖 DefaultConfig/env；manual 自动补 isolate；列表合并 Storage.ListSessions 与 live phase；缺 goal 400；workdir 冲突 409）、GET `/api/sessions/{id}`（live/历史树 JSON+phase）、GET `.../log`（offset/limit 行分页，单页上限 2000，含 total/has_more/next_offset）、POST `.../resume`（参数可选、workdir 从 tree 恢复并校验）。
  - 动作端点：`plan/approve`、`plan/adjust`、`abort`、`pause`、`redo`、`add`、`answer` 全部映射 RunHandle 动作；哨兵错误→409（ErrWorkdirBusy/SessionActive/RunFinished/WrongPhase），参数错→400，未知会话→404。
  - 审批端点：GET `.../approvals/{leafID}` 返回 FileChange diff 载荷；POST 同路径 decision=approve|reject+comment（非法 decision 400、reject 缺 comment 400）。RunHandle 新增 PendingApproval/PendingApprovalNodeIDs，leafApprovals 改存 pendingApproval{req,ch}；hook 注册与 phase=leaf_approval 同一把锁，消除「事件先于 map 注册」竞态。
  - `pkg/server/fs.go`（新）：GET `/api/fs/list`（支持 ?session= 取会话 workdir；目录优先排序、复用 tools.IsSkippedName 忽略规则、symlink 标记 escaped、2000 上限+truncated）与 `/api/fs/file`（1MiB 上限→413、NUL 二进制嗅探→415、目录/非常规文件 400）；约束双层：Sandbox.Resolve 词法防 `..`/绝对路径（400）+ EvalSymlinks 全链校验防 symlink 逃逸（403，兼容 macOS /var→/private/var）；内部 symlink 可正常跟随读取。
  - `pkg/tools/sandbox.go`：导出 `IsSkippedName` 供 web 文件 API 复用同一忽略集；`server.go` 加 WithWorkdir/WithClientFactory（测试注入 ScriptedClient）；`guided.go` review 每轮重发 plan_review pending（adjust 后前端恢复审批控件）；runmanager phase 跟踪不再把 planning/replan 的 DECOMPOSING 误判为 running。
  - 测试：`fs_test.go` 4 个（正常列举/读取+忽略隐藏、`..`/URL 编码/绝对路径 4xx、逃逸 symlink 403+列表标记+内部 symlink 200、二进制 415/超大 413/目录 400）；`api_test.go` 8 个（TR-5.1 create/list/get/log 分页/历史可读，TR-5.2 approve+adjust+abort HTTP 驱动，状态保护 404/409，workdir 冲突 409，pause→resume→完成 HTTP 闭环，manual 叶子审批 reject→redo→approve HTTP 闭环）；`go test ./... -race` 全绿，server 包连跑 5 轮稳定。
- **Description**:
  - POST /api/sessions（goal+参数+approval_mode）、GET /api/sessions（ListSessions+事件汇总）、GET /api/sessions/{id}（树 JSON）、GET .../events（Task 4）、GET .../log（分页读日志）。
  - POST .../approve|adjust|abort|pause|redo|add|answer；POST /api/sessions/{id}/resume。
  - GET .../approvals/{leafID}（diff 载荷）与 POST .../approvals/{leafID}（decision=approve|reject+comment）。
  - GET /api/fs/list?path= 与 /api/fs/file?path=：基于 sandbox Resolve 做 workdir 约束（拒 `..`/绝对路径/symlink 逃逸），列表套 skipDirNames，文件读取限大小与文本类型。
- **Acceptance Criteria Addressed**: AC-3, AC-5, AC-6, AC-7, AC-8
- **Test Requirements**:
  - `rule` TR-5.1: 会话 CRUD/resume 与列表字段正确。证据：httptest。
  - `rule` TR-5.2: approve/adjust/abort 三动作经 API 驱动 Guider 行为正确。证据：集成测试。
  - `rule` TR-5.3: 路径穿越（../、绝对、symlink）全部 4xx；正常读取 200。证据：安全测试用例。
  - `rule` TR-5.4: 大文件/二进制文件返回明确错误而不是崩溃。证据：边界测试。

## Task 6: `divvy serve` 接线
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 5
- **Completion Evidence**:
  - `cmd/divvy/main.go`：新增 `-serve`、`-port`（默认 0）、`-no-open`；互斥校验（与 -interactive/-guided/-plan/-resume/-sessions/-status/-report/-prune/-log/-events 及位置 goal 均报 mutually exclusive/no goal argument）；usage 增加 serve 行。
  - `cmd/divvy/serve.go`（新）：signal.NotifyContext(SIGINT/SIGTERM) → `RunManagerWithContext` → server.Start(port) 只绑 127.0.0.1；启动行打印 `URL/workdir/data`（URL 含随机 token）；darwin `open`/linux `xdg-open`/windows rundll32 自动开浏览器（-no-open 关闭，失败仅 warn）；信号后 `httpSrv.Shutdown` 先停止接单，`mgr.Shutdown(10s)` 等待所有运行中会话中断+checkpoint，退出码 0 并提示 resume 命令。
  - `pkg/agent/runmanager.go`：RunManager 增加 baseCtx/baseCancel 与 `NewRunManagerWithContext`、`Shutdown(grace)`（取消全部 run ctx 并按截止时间等 done）；attach 的 run ctx 改派生自 base；RunHandle 增加 aborted 标记——用户 Abort 判 failed，进程级中断（SIGINT/Shutdown）判 paused（可 resume）；serve 在 g.Run 因 context.Canceled 返回后、Close sinks 前显式 `checkpoint()`，保证叶子执行中途被杀也能落盘恢复。
  - README 增加「Web 界面（-serve）」小节（token/Host 安全模型、新建会话参数、apiKey 内存态、审批/暂停 resume、Ctrl+C 语义）。
  - TR-6.1 证据：`cmd/divvy/serve_test.go` 进程级测试（TestMain 哨兵 DIVVY_TEST_SERVE 让重执行的测试二进制进入 CLI 模式）：`-serve -port 0 -no-open` 启动 → 从启动输出正则取 URL → 无 token 401 / 带 token health 200 / 根路径占位页 200 → SIGINT → 15s 内退出码 0；`TestRun_ServeMutex` 覆盖 6 组互斥。另在 `pkg/agent/runmanager_shutdown_test.go` 确定性验证中断→树落盘→新 manager Resume 跑完产出 out.txt。TR-6.2：无 web/dist 时 `go build ./...` 成功（Task 4 的 go:embed 占位页兜底）；`go test ./... -race`、vet、gofmt 全绿。
- **Description**:
  - main.go 增加 `-serve` 与 `-port`（默认 0）、`-no-open`；启动服务后打印 `http://127.0.0.1:<port>/?token=...`，可选自动打开浏览器；信号处理：Ctrl+C 优雅关闭（保存运行中会话）。
  - help 文案与 README 增加 serve 用法；与现有 flag 模式做互斥校验。
- **Acceptance Criteria Addressed**: AC-1, AC-10
- **Test Requirements**:
  - `rule` TR-6.1: `-serve -port 0 -no-open` 进程启动，curl 带 token 访问健康检查 200；kill 后 running 会话已 checkpoint（可 resume）。证据：脚本/进程测试或手工走查记录。
  - `rule` TR-6.2: `go build ./...` 在 web/dist 缺失时成功。证据：CI Go job。

## Task 7: 前端脚手架与三栏应用壳
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 4（联调需要）, Task 5
- **Completion Evidence**:
  - 新 `web/` 子项目（依赖隔离在 web/，Go 主构建零 Node 依赖）：Vite 5.4 + React 18.3 + TypeScript 5.6（strict + noUnusedLocals）+ Tailwind 3.4 + zustand 4.5；`package.json`/`tsconfig.json`/`vite.config.ts`（dev 5173，/api 代理 VITE_DEV_TARGET 默认 127.0.0.1:8787）/tailwind/postcss/index.html；`web/.gitignore` 忽略 node_modules/dist/.vite（dist 按 Q2 留待 Task 11 提交+CI 一致性校验）。
  - `src/api/client.ts`：token 从启动 URL `?token=` 捕获→sessionStorage→Bearer 注入全部请求（清理地址栏）；ApiError 携带状态码映射；封装 sessions/plan/abort/pause/add/redo/answer/approvals 全部端点。
  - `src/api/sse.ts`：EventSourceClient——query 带 token、原生断线重连+1s→15s 指数退避兜底、Last-Event-ID 续传、按 seq 客户端去重、连接状态回调。
  - `src/store.ts`：zustand 单 store（sessions/currentId/live/events/sseStatus），SSE 事件驱动 phase 状态机（plan_review/leaf_approval/ask/state_change/session_end/user_pause），事件视图 1000 条上限（NFR-5），5s 轮询刷新会话目录。
  - UI（中文、深色 Cursor 风格）：`TopBar`（相位徽章+SSE 连接点+model/tokens/cost 占位，Task 8/9 填数）；左 `Sidebar`（新建按钮、会话/文件 tab、会话行含目标截断/叶子进度/live 相位/相对时间，文件 tab 留 Task 10）；中 `CenterMain`（空态 hero+新建会话卡片；选中态相位条+事件时间线，树可视化留给 Task 8）；右 `RightPanel`（节点/diff/日志 tab 空态，Task 8/9/10 填充）；`NewSessionForm` 完整字段：goal/model/baseURL/apiKey(password,autoComplete off)/并行数/成本/Token 预算/auto-manual 审批切换/isolate/git-commit/web/browser 开关（manual 说明 isolate 必需）；自研 SVG 图标集（零图标依赖）。
  - TR-7.1：`npm run build`（tsc -b 严格零错误 + vite build）通过，产物 index 0.41KB/CSS 18KB/JS 167KB(gzip 55KB)；`pkg/server/dist_test.go` 新增 TestStatic_ServesBuiltDist（dist 存在时断言 Go 服务真实 shell+哈希资产 200+SPA 回退，无 Node 环境 t.Skip 由占位页兜底）实测通过；Go 全量 `go test ./... -race` 绿。
  - TR-7.2（视觉 rubric）留待 Task 13 端到端走查截图；本任务已落实三栏信息架构、暗色主题、状态色与响应式滚动基础。
- **Description**:
  - web/：Vite + React 18 + TypeScript + Tailwind；API client（token 注入）、SSE client（自动重连+去重）、路由/状态用轻量内置方案（zustand 或 context，避免过重依赖）。
  - 三栏布局：左栏（会话列表/文件浏览 tab）、中栏（对话与任务树）、右栏（节点详情/diff/日志 tab）；深色主题，中文文案。
  - 新建会话表单：goal、model/baseURL/apiKey（密码框）、parallel/isolate/git-commit/web/browser、预算、审批模式。
- **Acceptance Criteria Addressed**: AC-2, AC-8
- **Test Requirements**:
  - `rule` TR-7.1: `npm run build` 通过且 tsc 无类型错误；dist 可被 Go 首页加载。证据：web job 构建日志。
  - `rubric` TR-7.2: 视觉与信息架构；scale 1-5；anchors 1=原始堆砌/3=可用但无层次/5=Cursor 级三栏与深色质感；threshold >=4；证据：截图走查。

## Task 8: 实时任务树与节点详情
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 4, Task 7
- **Completion Evidence**:
  - `src/types.ts` 补齐 TaskTree/TaskNode/TokenUsage/ErrorRecord/ContractSpec/DoD/NodeState/NodeType，与 Go 端 JSON tag 对齐。
  - `src/store.ts`：保存最新 tree_snapshot 为权威树（替换式），打开会话时从 GET detail 取树并默认选中 root；快照更新时保留仍存在的选中节点；新增 selectedNodeId/tree/selectNode。
  - `src/lib/tree.ts`：traverse（按 children_ids 稳定有序遍历）、treeStats（叶子进度/总调用/token/失败/进行中）、formatDuration（ms/s/m）、compactTokens（k/M）、stateMeta 七态颜色/标签/动画表。
  - `src/components/TaskTreeView.tsx`：递归树行（depth 缩进、折叠箭头、状态字形、标题、节点 id）、每行状态徽章 + 子节点数/重试数/token/耗时/端到端验收标记，失败节点行内红色错误摘要；可手动折叠，已完成且端到端验收的复合节点自动折叠；点击行选中（aria-treeitem/aria-selected/aria-expanded）。
  - `src/components/NodeDetail.tsx`（接入右栏）：头部状态/类型/depth/标题/描述；指标网格（重试/调用/token/耗时/prompt/completion）+ 端到端验收标记；契约四区（输入/产出/依赖/约束）；DoD（描述/命令/期望输出/超时）；结果摘要；错误历史倒序时间线（本地时间、fp 前缀、rose 卡片）。
  - `src/components/EventTimeline.tsx` 重构：17 种事件中文标签+彩色圆点，verify/tool_call/llm_call/leaf_approval/ask/retry 等按真实事件字段（ok/exit_code/command/call_kind/resp_tool_calls/files 等）出摘要，过滤噪声、上限 500 条并提示总量。
  - `src/components/CenterMain.tsx`：选中态改为「树头部统计（叶子 x/y·调用·token·失败/进行中·model）+ 树滚动区 + 可折叠事件流（带计数）」双区；历史会话标注只读。
  - TR-8.1 证据：构建严格通过（tsc -b + vite build，58 模块，JS 181KB/gzip 58.6KB）；真实联调走查（go serve 8799 + vite dev 5173 代理 + 浏览器自动化）：①三栏壳/新建表单/连接状态正常无 console 错误；②种入含 COMPLETED+重试1、RUNNING+重试2+2 条错误历史、PENDING 的三叶子演示树，树行正确渲染状态色/重试/token/耗时与头部统计（叶子1/3·调用22·23k tok·2 进行中·model 名）；③点击叶子 1.2 右栏正确显示指标、契约输入/产出/依赖、DoD 命令与 2 条带指纹的错误历史；④历史会话 SSE 补发树快照并显示「已连接」。
  - NFR-5：事件视图 500（store 保留 1000）、树按需折叠、所有长文本 break-words/line-clamp，支撑 200 节点/1 万事件浏览。
- **Description**:
  - 消费 SSE：tree_snapshot 增量渲染树（状态色/图标、retry 数、耗时、成本徽标），事件流时间线（llm_call/tool_call/verify/retry/gate 等图标化、可展开 preview）。
  - 节点详情面板：契约 inputs/outputs/constraints、DoD 命令、错误历史、结果摘要、token 用量。
  - 大列表保护：事件流虚拟渲染或上限 1000 条 + 「查看全部」。
- **Acceptance Criteria Addressed**: AC-2, AC-8
- **Test Requirements**:
  - `rule` TR-8.1: ScriptedClient 跑一个含失败重试的双叶任务，界面树状态/计数与后端最终树一致。证据：组件测试或手工走查记录。
  - `rubric` TR-8.2: 状态可读性；scale 1-5；anchors 1=要靠日志猜状态/3=状态可见但杂乱/5=一眼定位卡住叶子与原因；threshold >=4；证据：截图走查。

## Task 9: 审批、问答与运行控制界面
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 8
- **Completion Evidence**:
  - `src/lib/diff.ts`（新，零依赖）：splitLines + LCS DP（Int32Array，4000 行保护）行级 diff，computeDiff 产出 context/add/remove 行（带新/旧行号），toHunks 按 3 行上下文聚合 hunk，非 git 工作区同样可用。
  - `src/components/PlanReviewBar.tsx`：plan_review 阶段琥珀色审批条——批准执行/调整（展开意见框，提交触发 replan 后重新挂起）/中止（计划保存不执行），busy 态。
  - `src/components/FileDiffView.tsx` + `LeafApprovalCard.tsx`：leaf_approval 阶段紫红审查卡，按 修改→新增→删除 排序的文件清单（状态徽章、冲突标记、二进制/截断提示、可折叠）；修改文件渲染 unified hunks（双行号列 + +/- 着色，emerald/rose），新增/删除整文件着色，空变更与无差异有明确文案；GET approvals/{leaf} 带 0/150/400ms 重试覆盖注册竞态；拒绝必填意见（按钮禁用态）→ decide reject，同 leafId 多轮靠 phase 重入 effect 重新拉 diff；批准合并。
  - `src/components/AskDialog.tsx`：ask 阶段模态弹窗展示问题，回答输入（⌘/Ctrl+Enter），「暂停」或 /pause 文本走 pause 语义，独立暂停按钮。
  - `src/components/RunControls.tsx`：暂停（running/ask 可用）/中止（confirm 二次确认）/加叶子（内联指令输入，Enter 提交/Esc 取消，仅 running）/重做节点（对当前选中节点，按钮显示其 id，相位不对时禁用并提示）。
  - `src/components/ActionToast.tsx`：控制动作错误右下角 toast，6s 自动消失；`store.ts` 新增 planApprove/planAdjust/abort/pause/addInstruction/redoNode/answer/decideLeaf 八个 action（withRun 统一取当前会话+actionError 路由）；修复 phase 跟踪：session_end outcome 按后端 completed/paused/interrupted/failed 正确映射（此前误判 success 导致完成会话显示失败），leaf 决定后清 leaf_approving、ask 回答/恢复运行后清 pending_ask。
  - TR-9.1 证据（真实端到端，node mock OpenAI 端点 + go serve 8799 + vite 5173 + 浏览器自动化）：UI 填表单（mock baseURL+manual）创建会话→Plan 审批条出现且运行控制按相位禁用→批准→叶子卡显示 `out.txt 新增 + first`→拒绝意见「must say second」（空意见按钮禁用已验证）→重做后第二轮审批内容变为 second→批准→顶栏「已完成」、后端 session_end outcome=completed、工作区落盘 second；第二个会话还覆盖 modified 场景（同 workdir 已有文件），unified 行 diff 正确显示 `- second / + first` 与双行号，以及「重做后内容无变化→本次叶子没有文件变更」边界。修复 outcome 映射后重跑闭环，完成相位正确。Ask 弹窗渲染逻辑与 ask/pause API 已在 Task 5 集成测试覆盖。
  - 构建：tsc -b 严格零错误 + vite build（65 模块，JS 196KB/gzip 63KB，CSS 26KB）；Go 全量 `go test ./... -race` 绿。
- **Description**:
  - Plan 审批条（批准/调整输入框/中止）；叶子审批卡片（文件清单 + unified diff 高亮，逐文件展开，批准/拒绝+必填意见）。
  - ask 弹窗（问答）；顶部控制条：暂停、redo（节点操作菜单）、add 输入。
  - 会话页顶栏：状态、模型、累计 token/成本、耗时。
- **Acceptance Criteria Addressed**: AC-3, AC-4, AC-7, AC-8
- **Test Requirements**:
  - `rule` TR-9.1: manual 模式下从界面完成「拒绝→叶子重做→批准」闭环，workdir 最终内容正确。证据：端到端手工走查记录。
  - `rubric` TR-9.2: diff 审查体验；scale 1-5；anchors 1=看不懂改了啥/3=统一 diff 可读/5=并排/统一切换、文件导航顺畅；threshold >=4；证据：截图走查。

## Task 10: 会话管理与文件浏览
- **Status**: `completed`
- **Priority**: medium
- **Depends On**: Task 9
- **Completion Evidence**:
  - `src/types.ts`：新增 `FsEntry/FsEntryType/FsListResponse/FsFileResponse/LogPage`，字段与 `pkg/server/fs.go`、`handleSessionLog` 的 JSON tag 逐一核对一致（path/entries/truncated、name/path/type/size/mode/escaped、lines/offset/next_offset/total/has_more）。
  - `src/api/client.ts`：`fsList(path,session?)`、`fsFile(path,session?)`（?session= 自动取当前会话 workdir）、`log(id,offset,limit)`；`resumeSession` 复用既有 `POST /sessions/{id}/resume`。
  - `src/store.ts`：`resumeSession(id)`（成功后 refreshSessions+openSession，错误走 actionError）、`fileView {sessionId,path}` + `openFile/closeFile`，打开会话切换时重置文件视图。
  - `src/components/Sidebar.tsx`：会话 tab 五枚筛选 chip（全部/进行中/可续跑/完成/失败），`rowKind` 优先用 live.phase（done/failed/active），无 live 时按持久化 root_state（COMPLETED→done，FAILED/SKIPPED→failed，其余→resumable）；行展示目标截断/会话 id/叶子进度/中文相位/相对时间；「可续跑」行内嵌带 spinner 的「续跑此会话」按钮；空筛选有明确空态。
  - `src/components/FileTree.tsx`（左栏文件 tab）：根面包屑「工作区」+ 逐级路径（点击跳转）、「.. 上一级」、目录优先排序与大小（B/KB/MB）由后端给出；symlink 越界红色「越界」标记；2000 上限显示 truncated 提示；忽略规则（.git/node_modules 等）复用后端 `IsSkippedName`，前端不重复维护；未开会话时提示浏览的是服务默认工作区。
  - `src/components/FileViewer.tsx`（中栏只读查看器，替代会话视图）：等宽 `<table>` 行号列、头部路径+大小+行数+只读标记+关闭；友好降级面板——413「超过 1 MiB 上限」、415「二进制不支持预览」、403「软链接指向工作区外」；路径切换/取消用 cancelled 标志防竞态。
  - `src/components/LogView.tsx`（右栏日志 tab）：尾优先分页（PAGE=500，先取 total 再载最后一页），「更早」向前翻页（到 offset 0 禁用）、「刷新」从当前尾部增量拉新行；头部 `已载/总行` 计数；空日志/错误态。
  - 走查暴露并修复两个后端缺陷（前端行为依赖其正确性）：
    1. **终态树不落盘**：guided（web serve）路径在「全部完成」与「叶子失败/中止」时均未做最终 checkpoint（`Close()` 设计上不存盘），重启后已完成/失败会话在列表里被误判为「可续跑」、历史树停在 PENDING。修复：`pkg/agent/guided.go` `Run` 在 execute 返回后（nil 或非 errStdinClosed 的任何 err，含 pause/中断/失败）补一次 checkpoint；`pkg/agent/orchestrator.go` 非 guided `Run` 的验收失败/HasFailed/firstErr 返回路径同样补 checkpoint（完成路径原本已有）。
    2. **pause→resume 竞态（409）**：补的 checkpoint 放大了既有窗口——`user_pause` 事件经 applyEvent 提前把 phase 置为 paused，但 serve 尚未释放 workdir 锁，立即 resume 偶发 `ErrWorkdirBusy`（-race 下约 1/5）。修复：`RunManager.finish` 将「释放 busy 槽 + 终态 phase 发布」并入同一临界区（manager→handle 锁序，无反向嵌套）；`RunManager.Resume` 发现同会话存在 PhasePaused 旧句柄时先 `<-old.Done()` 等其完全收尾再 reserve。修复后该用例 -race 连跑 10 次全绿。
  - TR-10.1 证据（真实端到端：good mock :18999 / bad-worker mock :18998 + go serve 8799 + vite 5174 + 浏览器自动化，datadir `/tmp/d10-data`、workdir `/tmp/d10-ws`）：预置 3 个原子单叶会话——A 经 resume+批准跑成 COMPLETED、B 由「只 finish 不产出」的 bad mock 经 MaxStall 闸门 FAILED（错误历史含 fp 指纹）、C 保持 PENDING；**重启 serve（无任何 live 句柄）后仅按磁盘 root_state 分类仍为 A 完成 / B 失败 / C 可续跑**（修复前三者落盘均为 PENDING）。浏览器走查：五个筛选 chip 结果逐一正确（进行中显示空态文案）；打开 B 显示只读失败树+45 条事件回放+节点错误历史；C 的「续跑此会话」点击后立即在 plan_review 重新挂起并显示计划审批条与原 120 tok 树；日志面板对 607 行临时日志验证尾载 500/607→「更早」→607/607 且按钮禁用（API 侧 offset/next_offset/has_more 边界另验）。
  - TR-10.2 证据：API 与 UI 双层验证 `../etc/passwd` → 400、big.txt(1.1MB) → 413、blob.bin(含 NUL) → 415；文件列表不展示 .git/node_modules（后端忽略规则）；src 目录钻取、面包屑、内部文件读取（go.mod/main.go 行号视图）正常。
  - 构建与回归：`npm run build` tsc 严格零错误 + vite build（68 模块，JS 205.85KB/gzip 65.66KB，CSS 26.99KB）；`go build ./...`、`go vet`、gofmt、`go test ./... -race` 全绿（含 `TestAPI_PauseAndResume`、`TestAPI_LeafApprovalDiff` -race 各 10 连跑）。
- **Description**:
  - 左栏会话列表（goal、时间、进度、状态筛选）、打开历史会话（只读树+事件+日志）、未完成会话 resume 按钮。
  - 文件 tab：目录树（忽略规则）、文本文件只读查看器（等宽、行数、大小友好降级）。
- **Acceptance Criteria Addressed**: AC-5, AC-6, AC-8
- **Test Requirements**:
  - `rule` TR-10.1: 预置 3 个会话（1 完成、1 失败、1 可续跑），列表/详情/resume 均正确。证据：手工走查。
  - `rule` TR-10.2: 文件浏览无法越界，忽略目录不展示。证据：沿用 TR-5.3。

## Task 11: 构建整合与 CI
- **Status**: `completed`
- **Priority**: medium
- **Depends On**: Task 6, Task 7
- **Completion Evidence**:
  - `web/assets.go`（新，`package web`，与 web/ 同目录以满足 go:embed 不能跨 `..` 的限制）：`//go:embed all:dist` 把 Vite 产物编进二进制；导出 `DistFS() (fs.FS, bool)`，缺 index.html 时返回 false 让调用方回退占位页。
  - `cmd/divvy/serve.go`：启动时取 `web.DistFS()`，存在真实产物则 `server.WithStatic(dist)`，否则维持 `server.New` 内置占位页——同一份代码同时支持「有 dist」与「仅占位」。实测新二进制 `/` 返回含 `/assets/index-J5NzgETq.js` 的真实 shell（208443 字节，200）、CSS 200、SPA 路由 `/sessions/xyz` 回退 root div、`/api/health` 无 token 仍 401（静态免 token 与 API 鉴权互不影响）。
  - 提交首版构建产物 `web/dist/`（index.html + hashed JS 205.85KB/gzip 65.66KB + CSS 26.99KB）；`web/.gitignore` 改为只忽略 `node_modules/`、`.vite/`、`*.tsbuildinfo`、`*.local`，明确注释 dist 需跟踪。整个 `web/` 子项目（src/配置/package-lock）首次纳入版本库，共 41 个文件，node_modules 不入库（`git check-ignore` 验证）。
  - 根 `Makefile`（新）：`web-install`（按 package-lock 时间戳触发 `npm ci`）、`web-build`、`go-build`、`vet`、`test`、`test-race`、`clean`、`help`。
  - `.github/workflows/test.yml`：新增 `web` job（setup-node 20 + npm 缓存走 `web/package-lock.json` → `npm ci` → `npm run build` → **`git diff --quiet -- web/dist` 一致性闸门**，不一致打印 diff 并失败 → 仅装 Go 跑 `go build ./...` 与 `go test ./web/ ./pkg/server/`，证明无 Node 也能嵌入并服务已提交产物）；既有 test/race/lint/browser 四个 Go job 不安装 Node（保持 TR-11.1）；按工程约定全部 runner 从 `ubuntu-latest` 钉到 `ubuntu-24.04`。
  - 嵌入产物的测试：`web/assets_test.go`（断言嵌入树有 Vite shell 且其引用的每个 /assets 文件在嵌入 FS 内）与 `pkg/server/embed_test.go`（`WithStatic(web.DistFS())` 下根路径出 hashed 资产、未知 SPA 路径回退）——在纯 Go job 即可运行，是 TR-11.1 的直接证据；`pkg/server/dist_test.go`（Task 7，磁盘版）随本次一起入库。
  - TR-11.1 证据：`go build ./...`（全新、无 Node 参与，仅 Go 1.27）成功；`go test ./... -count=1` 全绿（含新 `web` 包与 embed 测试）；web job 在只装 Go 的步骤里验证嵌入服务。
  - TR-11.2 证据：连续两次 `npm run build` / `make web-build` 产出字节一致、文件名哈希不变（`index-J5NzgETq.js` / `index-Dlal5oCF.css`），`git diff -- web/dist` 为空，CI 一致性检查可通过。
- **Description**:
  - 提交 web/dist（占位或首版构建产物）；根 Makefile/脚本增加 web-build；go:embed 路径同时支持「有 dist」与「占位 index.html」。
  - CI 增加 web job：node 安装、npm ci/build，并校验 dist 与源码构建一致（git diff --exit-code web/dist）；Go job 保持无 Node。
  - .gitignore 调整（node_modules、vite 缓存忽略；dist 跟踪）。
- **Acceptance Criteria Addressed**: AC-10
- **Test Requirements**:
  - `rule` TR-11.1: 干净环境只有 Go 时 `go build ./... && go test ./...` 绿。证据：CI Go job。
  - `rule` TR-11.2: web job 构建并通过 dist 一致性检查。证据：CI web job。

## Task 12: Tauri 外壳（macOS dev 先行）
- **Status**: `completed`
- **Priority**: medium
- **Depends On**: Task 11
- **Completion Evidence**:
  - 新 `desktop/` Tauri v2 工程（与 web/、Go 主仓隔离）：`Cargo.toml`（tauri 2 + url + rfd + unix libc，release panic=abort/lto/strip）、`build.rs`、`tauri.conf.json`（identifier `com.divvy.desktop`、devUrl 5173、frontendDist `../web/dist`、`bundle.externalBin: binaries/divvy-sidecar`、app+dmg target、icns/png 图标）、`capabilities/default.json`（仅 core:default，webview 不暴露任何 Tauri 命令，一切走 sidecar HTTP）、`package.json` 固定 `@tauri-apps/cli` 2.12。
  - `desktop/scripts/build-sidecar.sh`：`go build ./cmd/divvy` 到 `binaries/divvy-sidecar-<triple>`，按 `$GOOS/$GOARCH` 映射 aarch64/x86_64-apple-darwin、x86_64/aarch64-unknown-linux-gnu、x86_64-pc-windows-msvc（windows 补 .exe）；`scripts/dev.sh`：先构建 sidecar、再以 `VITE_DEV_TARGET=http://127.0.0.1:8799` 起 vite。tauri.conf 的 beforeDevCommand(beforeDev script)/beforeBuildCommand(npm run build in ../web)/beforeBundleCommand(sidecar 脚本) 全部接线，`tauri dev` 与 `tauri build` 均自动完成。
  - `desktop/src/sidecar.rs`（核心，可无头集成测试）：① `resolve_sidecar`——`DIVVY_SIDECAR` 覆盖优先；debug 用 manifest 下三重后缀文件；release 先找主程序同目录基础名（实测 Tauri v2 在 macOS 放 `Contents/MacOS/divvy-sidecar`、去后缀），再找 `<resources>/binaries/` 兜底；找不到给中文修复指引。② `launch_sidecar`——独立进程 spawn（stdin null、stdout/stderr piped），常驻线程持续 drain 两路输出防管道阻塞，从 banner 解析 `http://127.0.0.1:<port>/?token=`（dev 固定 8799 与 vite 代理对齐，release `-port 0` 随机），30s 超时与进程提前退出分别给明确错误，失败即 kill+wait 不留孤儿。③ `graceful_shutdown`——unix 先发 SIGINT 让 Go 侧 checkpoint，12s 宽限轮询 try_wait，超时 SIGKILL 兜底（Windows 走 TerminateProcess）。
  - `desktop/src/main.rs`：setup 阶段建 `app_data_dir/{,workspace}`（即 `~/Library/Application Support/com.divvy.desktop/`）→定位并启动 sidecar→debug 窗口开 `http://localhost:5173/?token=<解析值>`（token 仅走 query，vite 代理 /api），release 直接开 sidecar URL（内嵌 UI）；1280×820、最小 960×600；`RunEvent::ExitRequested` 触发 shutdown；setup 任何失败弹 rfd 原生错误框（「divvy 无法启动」）并返回 Box<dyn Error> 使启动非零退出。环境变量排障：`DIVVY_SIDECAR/DIVVY_SIDECAR_PORT/DIVVY_SIDECAR_STARTUP_TIMEOUT_MS`。
  - 图标：自研 1024×1024 PNG 源（纯 Node zlib 生成深色圆角底+分叉节点图案，无外部依赖），`tauri icon` 生成全套 icns/ico/png/android/ios 占位图标入库；`desktop/.gitignore` 忽略 node_modules/target/gen/sidecar 二进制（仅保留 `binaries/.gitkeep`），Cargo.lock 入库。
  - TR-12.1 证据（macOS arm64，Rust 1.99 + Go 1.27 + Node 24 + tauri-cli 2.12.1 + good mock LLM :18999）：`tauri dev` 一键完成「go sidecar 构建→vite→cargo 构建→开窗」，日志输出 `URL http://127.0.0.1:8799/?token=...`；进程树确认 sidecar PPID=应用 PID，WKWebView 子进程与 vite 5173 ESTABLISHED；经该 sidecar HTTP 创建会话（mock baseURL/key）→plan_review→批准→`done COMPLETED`，out.txt 落盘 app workspace、树持久化 COMPLETED。**人工真实关窗（窗口红钮）后核验**：`pgrep divvy-desktop`、`pgrep divvy-sidecar-aarch64`、vite 全空，8799/5173 无监听，日志 `[shell] sidecar exited (exit status: 0)`——关窗即带走 sidecar，无残留。
  - TR-12.2 证据：`sidecar.rs` 内 3 个 `cargo test` 集成测试（真实/假 sidecar）：`launch_parse_and_graceful_shutdown`（真 sidecar 解析 URL+token、原始 TCP 请求带 token 的 /api/health 返回 200 ok、SIGINT 后 exit 0 且 pid 消失）、`startup_timeout_is_reported_and_killed`（假脚本只 sleep，1.2s 超时返回「解析超时」、pgrep 无存活）、`early_exit_is_reported`（假脚本 exit 3，返回「提前退出」）；`cargo test` 3/3 绿。
  - 发布构建（TR-12 附带验证 release 注入路径）：`tauri build` 产出 `divvy.app` 23.83 MiB 与 `divvy_0.1.0_aarch64.dmg` 12.80 MiB；sidecar 实测位于 `Contents/MacOS/divvy-sidecar` 并被应用成功拉起（`-port 0` → 随机端口 63095）；该端口提供内嵌真实 UI（root 200、hashed JS 208443 字节、SPA 回退、无 token /api 401、带 token health 200）；release 应用终止同样 sidecar exit 0 无残留。`cargo fmt --check` 通过。
  - README 新增「桌面应用（Tauri，macOS）」小节：Xcode CLT/Rust/Go/Node 前置、`npm run dev`/`npm run build` 步骤、产物位置、sidecar 命名与排障环境变量。
  - 环境注记：共享卷（SMB）上 Rust 产物链接会损坏（cgu.o file is empty），构建时 `CARGO_TARGET_DIR` 指向本地磁盘（如 `~/divvy-desktop-target`）即可；该变量仅本机使用，未写入仓库。
- **Description**:
  - app/（或 desktop/）：Tauri v2 工程；externalBin sidecar 指向按平台命名的 divvy 二进制（构建脚本先 go build）；beforeDevCommand 启 vite，devUrl 走 vite，生产由 sidecar URL 注入（启动输出解析端口/token）。
  - 应用生命周期：启动 sidecar、关窗即 kill 进程组；窗口标题与图标占位。
  - README 写明 macOS 前置（Rust 工具链、Node、go）与 `tauri dev` 步骤。
- **Acceptance Criteria Addressed**: AC-9
- **Test Requirements**:
  - `rule` TR-12.1: macOS 上 `tauri dev` 开窗显示界面，可完成一次新建会话→审批→完成；关闭窗口后 sidecar 进程不存在（pgrep 验证）。证据：手工走查记录。
  - `rule` TR-12.2: 端口/token 解析失败时有明确错误提示，不留僵尸进程。证据：故障注入手工验证。

## Task 13: 端到端走查与 Review 修复
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 10, Task 12
- **Description**:
  - 全量 `go test ./... -race`、gofmt/vet、golangci-lint；npm build；浏览器与 Tauri 双形态走查全部 AC；独立 Review（fresh context）并按 review.md 修复。
- **Acceptance Criteria Addressed**: AC-1~AC-11
- **Test Requirements**:
  - `rule` TR-13.1: 全部 AC 具备独立证据（测试/截图/走查记录），CI 全绿。证据：review.md。
  - `rubric` TR-13.2: 整体完成度；scale 1-5；anchors 1=仅后端可用/3=主路径可用但有断点/5=浏览器与 Tauri 双形态主流程顺滑；threshold >=4；证据：独立 Review 评分。
