# desktop-ui（Tauri + React）独立代码审查报告

- 审查日期：2026-10-10
- 审查人：独立代码审查（fresh context，未参与本特性开发）
- 审查对象：divvy 仓库 `origin/master..master` 的 4 个提交 + 当前未提交工作区改动
- 规格基线：[spec.md](file:///Volumes/My%20Shared%20Files/project/divvy/.trae/specs/desktop-ui/spec.md)（AC-1~AC-11）、[tasks.md](file:///Volumes/My%20Shared%20Files/project/divvy/.trae/specs/desktop-ui/tasks.md)

---

## 1. 审查范围与方法

### 1.1 提交与改动范围

`git log --oneline origin/master..master`（4 个提交）：

```
46ec242 fix: 修复 leaf 审批 phase 注册竞态并清理 golangci-lint 问题
7d4b1bb fix(agent): serve/guided 终态树落盘并修复 pause→resume 的 workdir 锁竞态
c783c63 feat(desktop): Tauri v2 外壳，sidecar 生命周期管理与 macOS 打包
752d81b build(web): 提交前端产物并嵌入 Go 二进制，CI 增加 web 构建一致性 job
```

未提交工作区改动（`git status --short`）：

```
 M pkg/agent/events.go           # NewEventRecorder 扫描盘内最高 seq，跨 resume 延续序号
 M pkg/agent/eventsbus_test.go   # TestEventRecorder_SeqContinuesAcrossOpen
 M pkg/agent/runmanager.go       # Resume 在 Load 前等待旧 paused 句柄；DecideLeaf 先翻 phase
 M pkg/server/api_test.go        # TestAPI_ResumeSSESeqNoCollision + maxPersistedSeq
 D web/dist/assets/index-J5NzgETq.js
 M web/dist/index.html
 M web/src/store.ts              # openSession detail/SSE 相位调和 + phaseRank
?? web/dist/assets/index-BNoNqOAY.js
```

### 1.2 实际执行的命令（均在仓库根，Go 已按要求 `unset GOROOT` 并使用 /Users/zx/homebrew/bin/go，Node 使用 /Users/zx/.local/node/bin/node）

| 命令 | 结果 |
| --- | --- |
| `git log --oneline origin/master..master`、`git status --short`、`git diff`（全量）、`git diff --stat/--numstat` | 已逐行阅读 |
| `go build ./...` | 通过（BUILD_OK） |
| `go vet ./...` | 通过（VET_OK） |
| `go test ./pkg/agent/ ./pkg/server/ -count=1` | agent 11.9s / server 4.0s，均 ok |
| `go test -race ./pkg/agent/ ./pkg/server/ -count=1` | 均 ok |
| `go test ./... -count=1` | 全部 9 个包 ok（含 cmd/divvy 进程级 serve 测试） |
| `cd web && npm run build` | tsc 严格检查 + vite 构建成功；产物 index-BNoNqOAY.js（206.28KB）/ index-Dlal5oCF.css（26.99KB），与工作区 dist 哈希一致，证明 dist 确为当前源码的真实重建 |
| `grep -r sk-sentinel /tmp/d13-data` | 无匹配（exit 1），AC-11 良性流证据复核通过 |
| 自构复现（见 1.4）：`go build -o /tmp/d13-review-divvy ./cmd/divvy` + node mock + 脚本化 HTTP 走查 | 发现 P1-1（apiKey 经叶子 bash 输出落盘） |
| python3 解析 S3 历史事件文件 seq 布局 | 验证 highestEventSeq 在含历史重复 seq 的文件上取全局最大值 |

### 1.3 实际阅读的主要代码文件

- 后端：[events.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/events.go)、[runmanager.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/runmanager.go)、[orchestrator.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/orchestrator.go)（executeLeaf 全段、recordSessionStart/End）、[guided.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/guided.go)、[webguided.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/webguided.go)、[approval.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/approval.go)、[treebroadcast.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/treebroadcast.go)
- 服务端：[server.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/server.go)、[sse.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/sse.go)、[sessions.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/sessions.go)、[fs.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/fs.go)、[redact.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/redact.go)、[assets.go](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/assets.go)
- Tauri：[sidecar.rs](file:///Volumes/My%20Shared%20Files/project/divvy/desktop/src/sidecar.rs)、[main.rs](file:///Volumes/My%20Shared%20Files/project/divvy/desktop/src/main.rs)、[tauri.conf.json](file:///Volumes/My%20Shared%20Files/project/divvy/desktop/tauri.conf.json)、[default.json](file:///Volumes/My%20Shared%20Files/project/divvy/desktop/capabilities/default.json)
- 前端：[store.ts](file:///Volumes/My%20Shared%20Files/project/divvy/web/src/store.ts)、[sse.ts](file:///Volumes/My%20Shared%20Files/project/divvy/web/src/api/sse.ts)、[client.ts](file:///Volumes/My%20Shared%20Files/project/divvy/web/src/api/client.ts)、[TaskTreeView.tsx](file:///Volumes/My%20Shared%20Files/project/divvy/web/src/components/TaskTreeView.tsx)、[LeafApprovalCard.tsx](file:///Volumes/My%20Shared%20Files/project/divvy/web/src/components/LeafApprovalCard.tsx)
- 构建/CI：[test.yml](file:///Volumes/My%20Shared%20Files/project/divvy/.github/workflows/test.yml)、[Makefile](file:///Volumes/My%20Shared%20Files/project/divvy/Makefile)、[web/assets.go](file:///Volumes/My%20Shared%20Files/project/divvy/web/assets.go)
- 走查证据：检查截图目录 `/Users/zx/.trae-cn/trae-browser-screenshots/6ac97b91922792be8ee23bb8/`（共 56 张，2026-10-09 23:50 ~ 2026-10-10 00:35），实际放大查看其中 14 张关键截图。说明：截图文件名不含 `d13-` 字样（任务描述如此，但实际命名为 `shot-时间戳.jpg`）；`d13-` 前缀体现在 /tmp 产物（d13-data、d13-ws、d13-divvy、d13-*.mjs）。多数截图带浏览器自动化的元素标签（eNN 框、步骤气泡），是自动化 Snapshot 模式所致，但 UI 本体渲染可辨。

### 1.4 独立实证：叶子 bash 是否可把环境 apiKey 落盘

为检验 NFR-1「apiKey 不写入 events/log」在弱模型真实行为下是否成立，构造最小复现（产物均在 /tmp，未改动仓库任何文件）：

- Mock：[/tmp/d13-envinfo.mjs]，叶子动作序列为 `run_bash: echo "key=$OPENAI_API_KEY"` → write_file → finish（「echo/printenv 环境变量」是弱模型排查配置时极常见的动作）；
- 服务：用当前源码重新构建的 `/tmp/d13-review-divvy -serve -port 18795`，以 `OPENAI_API_KEY=sk-sentinel-secret-key-1234567890` 启动，脚本 [/tmp/d13-env-scenario.sh] 完成建会话→批准→done；
- 结果：`grep -rn sk-sentinel /tmp/d13-env-data` → **LEAK=YES**，明文 key 出现在事件文件 `/tmp/d13-env-data/events/sess_20261010_085558.jsonl` 的 seq 10（tool_call.output_preview）与 seq 11/13（llm_call.messages 中回灌给模型的 tool result preview）。详见 P1-1。

---

## 2. AC-1~AC-11 证据核对表

| AC | 独立可追溯证据 | 结论 |
| --- | --- | --- |
| AC-1 serve 可用且仅本机可访问 | `TestAuth_TokenRequired`（无/错 token 401）、`TestAuth_HostAllowlist`（恶意 Host 403、loopback 形态 200）、`TestStart_LoopbackAndURL`（实测 `127.0.0.1` 绑定与 token URL）、`TestStatic_PlaceholderWithoutToken`；`cmd/divvy/serve_test.go` 进程级测试（启动→取 URL→401/200→SIGINT 退出码 0）；本人复现时实际观察到 banner 行 `URL http://127.0.0.1:18795/?token=...`；代码 [server.go:253-270](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/server.go#L253-L270) 硬编码 127.0.0.1 绑定 | **pass** |
| AC-2 发起目标并实时看到树流转 | `TestSSE_ReplayLiveHeartbeatAndReconnect`（断言 state_change + tree_snapshot + 心跳 + 断线重连）、`TestAPI_ResumeSSESeqNoCollision`（resume 后新事件 seq 唯一且更大）；截图：000848/000919 等可见 live 统计与树行（叶子进度、调用、Token、状态色）；事件先订阅后补发，见 [sse.go:71-126](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/sse.go#L71-L126) | **pass** |
| AC-3 plan 审批三动作 | `TestAPI_PlanActions`（abort→failed 且不产出 out.txt；adjust→重新挂起；approve→done）、`TestRunManager_AbortAtReview/AdjustThenApprove`；截图：S3 列表最终显示「失败 0/1」、S4/S5 审批条（批准执行/调整/中止）及控件按相位禁用；语义与终端 guided 一致（同一 Guider.review 路径） | **pass** |
| AC-4 叶子 diff 批准合并/拒绝重做 | `pkg/agent/approval_test.go` 4 测试（TR-2.1 auto 零中断 / 2.2 批准合并 / 2.3 拒绝重做）、`pkg/tools/mirror_test.go` PreviewChanges（copy/worktree 两形态）、`TestAPI_LeafApprovalDiff`（reject→redo→approve HTTP 闭环）；截图 000745/000836 清晰显示 out.txt「修改」、重试 1、720 tok、1m47s，000919 显示 1/1 完成。拒绝路径关旧 mirror→新快照→意见进 prevError，见 [orchestrator.go:791-814](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/orchestrator.go#L791-L814) | **pass** |
| AC-5 历史会话列举/查看/续跑 | `TestAPI_CreateListGet`、`TestAPI_PauseAndResume`、`TestRunManager_ShutdownCheckpointsRunningLeaf`（中断→落盘→新 manager resume 产出 out.txt）；截图：五个筛选 chip、S6 失败树+事件回放+错误指纹、日志 68/68 行分页、002913 S4 审批条；列表合并 live phase 见 [sessions.go:185-213](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/sessions.go#L185-L213) | **pass** |
| AC-6 文件浏览沙箱 | `TestFS_PathTraversalRejected`（`..`/URL 编码/绝对路径 4xx）、`TestFS_SymlinkEscapeRejected`（逃逸 403、列表 escaped 标记、内部 symlink 200）、`TestFS_BinaryAndOversizedRejected`（415/413）；词法 Resolve [sandbox.go:114-130](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/tools/sandbox.go#L114-L130) + 全链 EvalSymlinks [fs.go:237-255](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/fs.go#L237-L255) | **pass** |
| AC-7 ask 问答可达 | 正向：`TestRunManager_AnswerQuestion`（真实 Guider/worker：ask→Answer→继续写文件，且断言事件流含问题与答案；含「答案在同 turn 后续 user 消息」防重问逻辑）；界面：截图 002950 AskDialog「Which file should I create?」+ S4 最终 1/1 完成；HTTP 层只有负向测试（review 相位 answer→409），**缺一条 pkg/server HTTP 正向集成测试**，但功能证据已覆盖端到端 | **pass**（附注：建议补 HTTP 正向用例，见 P2-8） |
| AC-8 界面可用性（rubric，threshold≥4） | 本人直接查看截图：三栏布局清晰、深色 Cursor 风格、中文文案准确、状态色/徽章统一、审批条与 diff 卡信息完整；235046 无标签截图显示空态+新建表单质感良好。事件视图 1000 条上限、长文本 line-clamp/break-words、树可折叠，满足 NFR-5 的基本保护。本人评分 **4/5**：信息架构与主路径达到 4 分，未见 5 分级的精细打磨（无虚拟列表、部分面板信息密度偏高） | **pass（4/5）** |
| AC-9 Tauri macOS 可启动 | 证据来自开发方走查记录（tasks.md TR-12.1：`tauri dev` 一键开窗、PPID 验证、经 sidecar 跑成会话、人工关窗后 pgrep 全空、端口无监听；release `tauri build` app/dmg、随机端口、关窗 exit 0）；`sidecar.rs` 内 3 个 cargo 集成测试（解析+健康检查+SIGINT 收尾、超时 kill、提前退出报错）。**独立审查人未重新执行 tauri（成本高），证据细节具体、与代码一致，予以采信但标注未独立复跑** | **pass（采信走查）** |
| AC-10 无 Node 全量构建不中断 | 本人独立执行：`go build ./...`、`go vet ./...`、`go test ./... -count=1` 全部通过，全程无 Node 参与；[test.yml](file:///Volumes/My%20Shared%20Files/project/divvy/.github/workflows/test.yml) 四个 Go job 均不装 Node，web job 另设 dist 一致性闸门与纯 Go embed 测试；占位页 [placeholder/index.html](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/placeholder/index.html) 兜底 | **pass** |
| AC-11 机密不落地 | 字面场景（界面填入自定义 apiKey）：`TestSecretsNeverTouchDisk`（DataDir 落盘扫描 + SSE 帧双断言）、所有 SSE 帧经 [writeSSEEvent→RedactBytes](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/sse.go#L183-L192)、`grep sk-sentinel /tmp/d13-data` 无结果——该字面口径通过。但 NFR-1 的绝对口径（apiKey 不写入 events/log）在「环境变量 apiKey + 叶子 echo/printenv」的现实场景下被本人实证击穿（P1-1） | **weak** |

---

## 3. 发现的问题

### P0（阻断）

未发现 P0 问题：无外部监听、鉴权链完整、无已知崩溃/数据损坏路径，全部测试（含 -race）通过。

### P1（重要）

#### P1-1：环境变量来源的 apiKey 可经叶子工具输出落盘到事件文件，违反 NFR-1

- 位置：
  - 工具子进程继承 serve 全量环境、未做 Env 过滤：[pkg/tools/sandbox.go:589-595](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/tools/sandbox.go#L589-L595)（`exec.CommandContext` 未设置 `cmd.Env`）；
  - 工具输出在掩码之前即持久化：tool_call 的 `output_preview` 落盘见 [events.go:429-448](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/events.go#L429-L448)（写入发生在 [events.go:210-220](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/events.go#L210-L220)），回灌模型的 tool result preview 落盘见 [events.go:252-253](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/events.go#L252-L253)；
  - 掩码仅作用于 SSE 出站：[sse.go:184](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/sse.go#L184)。
- 问题描述：FR-12 约定 LLM 配置默认沿用环境变量，`OPENAI_API_KEY` 存在于 serve 进程环境；叶子 bash 由该进程 fork，因此 `echo $OPENAI_API_KEY`、`printenv`、`env`（弱模型调试配置、诊断「为什么 401」时的高频动作）会把密钥读入工具输出，随后被**明文写入 events/*.jsonl**。本人用当前源码构建的二进制 + node mock 最小复现：seq 10 的 tool_call 事件与 seq 11/13 的 llm_call 消息预览中均出现完整 `sk-sentinel-secret-key-1234567890`（LEAK=YES）。
- 为何是问题：NFR-1 明确要求「apiKey 不写入 events/log」，AC-11 要求磁盘会话数据无 apiKey 明文；现有 `TestSecretsNeverTouchDisk` 只覆盖良性流（叶子不读环境），留下现实可触发的缺口。密钥一旦随事件文件进入备份/同步目录即不可逆扩散。注意：界面手工填入的自定义 apiKey 不在子进程环境中，不受此路径影响——受影响的是更常见的 env 配置路径。
- 建议修复（两层，可只先做第一层）：
  1. **子进程环境清洗（主修复）**：在 serve/RunManager 路径让工具沙箱以白名单/黑名单方式构造 `cmd.Env`，至少剔除当前生效的密钥变量（`OPENAI_API_KEY` 及 cfg 中已知 secret 名）；worker 的工具合法上不需要 LLM key。为满足 NFR-3，批处理 CLI 保持旧行为，清洗仅在 web serve/guided-web 路径启用（如在 Sandbox 增加 `ScrubEnv []string` 由 RunManager 设置）。
  2. **落盘前 scrub（纵深防御）**：让 EventRecorder 持有本 run 的 SecretValues，在 Record 内对会回显工具输出的字段（output_preview、messages preview）先替换再持久化；同时使 P1 场景在测试中可断言。
  3. 新增一条回归测试：复用本次复现脚本形态（mock 叶子 echo `$OPENAI_API_KEY`），断言事件文件与日志无明文。

### P2（建议）

#### P2-1：RunManager.Resume 在 reserve/校验失败时泄漏 Orchestrator 文件句柄

- 位置：[pkg/agent/runmanager.go:175-182](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/runmanager.go#L175-L182)。
- 问题：`Load` 返回的 `o` 已打开 EventRecorder（append 句柄）；随后 workdir 不匹配（176-178）或 `m.reserve` 失败（180-182，如旧句柄尚在收尾、busy 槽未释放）均直接 `return nil, err`，未调用 `o.Close()`。
- 为何是问题：每次踩竞态窗口的 resume 请求泄漏一个文件句柄（依赖 os.File 的 GC finalizer 兜底，时间不可控）；在 Windows 上还可能锁住会话文件。属资源泄漏而非行为错误。
- 建议：两个错误返回前先 `o.Close()`（参照 attach 内注册碰撞路径 244-249 的写法）。

#### P2-2：MaskSecret 为死代码，tasks.md 的自述与实现不符

- 位置：[pkg/server/redact.go:32-41](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/redact.go#L32-L41)。
- 问题：全仓 grep 确认 `MaskSecret` 除自身单测外无任何生产调用方；tasks.md TR-4 声称其用于「启动/状态行」，实际启动行（serve.go:53）打印的是完整 token URL（设计如此，一次性随控制台暴露给本机用户）。
- 建议：要么在真正需要人读的状态行（如对外日志中的脱敏摘要）接线使用，要么删除以免误导后续审查。

#### P2-3：密钥掩码只覆盖 SSE，审批/文件/日志三个出站 HTTP 路径未掩码

- 位置：审批载荷 [sessions.go:355-360](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/sessions.go#L355-L360)；文件内容 [fs.go:223-229](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/fs.go#L223-L229)；日志分页 [sessions.go:440-446](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/server/sessions.go#L440-L446)。
- 问题：若叶子把密钥写进了文件（或日志行含密钥），GET approvals 的 changes（NewContent/OldContent）、fs/file 的 content、log 的 lines 均为明文返回，未使用 live 句柄的 SecretValues。
- 为何是问题：与 P1-1 同源的纵深防御缺口；这三个接口都是带 token 的出站路径，掩码口径应一致。
- 建议：在对应响应体上调用 `Redact/RedactBytes(..., live.SecretValues()...)`（历史会话无 live 句柄时跳过——其文件已落盘，修复点在 P1-1）。

#### P2-4：openSession 相位调和存在「旧高秩相位短暂覆盖新低秩相位」的瞬态窗口

- 位置：调和逻辑 [web/src/store.ts:111-132](file:///Volumes/My%20Shared%20Files/project/divvy/web/src/store.ts#L111-L132)；排序表 [store.ts:264-279](file:///Volumes/My%20Shared%20Files/project/divvy/web/src/store.ts#L264-L279)。
- 问题：断线重连/SSE 补发不区分 replay 与 live，历史的 plan_review pending（rank 3）、leaf_approval（4）、ask（5）在补发流中会短暂压过当前 running（rank 1）；detail GET 若恰在补发推进到后续 state_change 之前返回，调和条件 `phaseRank(sseLive) >= phaseRank(detail)` 会保留旧相位，审批条/审批卡出现「闪回」。另外 [store.ts:120-122](file:///Volumes/My%20Shared%20Files/project/divvy/web/src/store.ts#L120-L122) 的 `else if (!live) live = sseLive` 未校验 `sseLive.session_id === id`。
- 为何只是 P2：所有追踪过的时序中补发帧按文件顺序在同一 TCP burst 内连续派发，后续 state_change/session_end 到达后状态自愈，用户可感知时间为毫秒级；未发现永久卡死的组合。但长时静默叶子（如长时间 bash）期间闪回可能停留稍久。
- 建议：服务端在补发帧打 `replay: true` 标记，客户端只对 live 帧做相位机更新（补发仅用于历史浏览）；并在 else-if 分支补 `session_id === id` 校验。

#### P2-5：highestEventSeq 的两个边界（>16MB 行、文件截断行）

- 位置：[pkg/agent/events.go:144-173](file:///Volumes/My%20Shared%20Files/project/divvy/pkg/agent/events.go#L144-L173)。
- 问题 1：scanner 单行上限 16MB；若 JSONL 中存在 >16MB 的行（只有 tree_snapshot 可能这么大），`Scan` 立即返回 ErrTooLong 并终止整个扫描；当该行之后还有更高 seq 行时，最高 seq 被低估，resume 后新 seq 与旧行碰撞。现实规模（200 节点树 JSON 远小于 16MB）下不可达。
- 问题 2：文件末尾存在截断行（崩溃半写）时该行被跳过，新计数器复用其 seq N，导致同一文件内含一条截断的 seq N 残行与一条完整 seq N 新事件。当前所有读取方（tailLines/replayEvents/SummarizeEvents）均容忍坏行，功能无碰撞，但文件不变量被破坏。
- 另：>1MB 时从 `size-1MiB` 起扫，首行可能落在一行中间——该行必然 JSON 解析失败被跳过，且最高 seq 必在尾部，**该偏移本身不会漏读最高 seq，未发现问题**。
- 建议：打开时将文件截到最后一个完整换行边界（消除截断行复用）；scanner 遇 ErrTooLong 时扩容重读而非终止。

#### P2-6：Tauri sidecar 的强杀路径按单 pid 而非进程组，个别极端时序可能留孤儿；Windows 无优雅退出

- 位置：强杀 [desktop/src/sidecar.rs:279-284](file:///Volumes/My%20Shared%20Files/project/divvy/desktop/src/sidecar.rs#L279-L284)、宽限后 kill [sidecar.rs:270-276](file:///Volumes/My%20Shared%20Files/project/divvy/desktop/src/sidecar.rs#L270-L276)。
- 问题 1：`kill`/SIGINT 都发给 sidecar 单个 pid。正常 SIGINT 路径下 Go 侧 context 取消会清掉叶子工具的 Setpgid 进程组（走查 pgrep 为空已验证）；但若 12s 宽限耗尽才 SIGKILL、而某工具组尚未被 Go 清理，则该组被 launchd 收养成为孤儿。启动超时场景此时还没有工具子进程，不受影响。
- 问题 2（Windows）：`graceful_shutdown` 的 SIGINT 仅 cfg(unix)，Windows 下每个退出都空等 12s 再 TerminateProcess，没有 checkpoint。本期明确不支持 Windows，仅需注释说明或后续补 ctrl-break。
- 建议：spawn 时用 `CommandExt::process_group(0)` 让 sidecar 独立成组，收尾向 `-pid` 发信号/杀整组；Windows 路径注明语义。

#### P2-7：setup 在 sidecar 启动成功后、窗口建成前失败时，sidecar 可能无人收尾；release 无 CSP

- 位置：[desktop/src/main.rs:33-52](file:///Volumes/My%20Shared%20Files/project/divvy/desktop/src/main.rs#L33-L52)；CSP [desktop/tauri.conf.json:24-26](file:///Volumes/My%20Shared%20Files/project/divvy/desktop/tauri.conf.json#L24-L26)。
- 问题：33 行 launch 成功、34 行入 state 后，若 URL 解析或窗口构建返回 Err，setup 中止；`RunEvent::ExitRequested` 是否在启动失败路径触发并不确定，sidecar 存在孤儿风险。另外 `csp: null` 使 WebView 无内容安全策略（内容虽为本机 UI，dev 下还含 vite 服务面）。
- 建议：在 fatal 返回前显式调用 `app.state::<SidecarState>().shutdown()`；release 配置最小 CSP（`default-src 'self'; connect-src 'self' http://127.0.0.1:*`）。

#### P2-8：AC-7 缺 pkg/server HTTP 正向集成测试；NFR-5 大流量性能无实测证据

- 位置：现有 ask 正向覆盖仅在 pkg/agent（TestRunManager_AnswerQuestion），pkg/server 的 answer 只有负向 409 用例。
- 建议：补一条 httptest 用例（脚本叶子 ask→POST answer→完成，断言 ask_pending/ask 事件），与 spec「server 集成测试」证据口径对齐。另：200 节点/1 万事件的性能（NFR-5）目前仅有截断上限等结构性保护，无实测数据，建议后续补一次规模化走查；不阻断本期。

### 经检查未发现问题的项目

- RunManager 锁序：全部持 h.mu 的位置（Snapshot/applyEvent/handleLeafApproval/DecideLeaf 等）均不反向获取 mgr.mu；finish 的 mgr→handle 嵌套无死锁路径。
- finish 临界区：busy 槽删除与终态 phase 发布在同一临界区，「phase 终态 ⇒ workdir 可再预约」不变量成立。
- Resume 等待时序：对 PhasePaused 旧句柄在 Load 之前等待 Done，正确覆盖了「Load 内 openSinks 读最高 seq」与 busy 槽两个竞态；S3 事件文件实证表明 highestEventSeq 在含历史重复 seq 的文件上取全局最大值（跳过中段 1-5 重复块、从 9 续到 10）。
- leaf 审批 map 注册与 phase 原子性：handleLeafApproval 同锁注册；DecideLeaf/Answer 在信号/提交前翻回 running，「phase=leaf_approval 但 GET 404」窗口已闭合。
- SSE 补发/实时：先订阅后读文件、按 seq 去重、Last-Event-ID 过滤、id-less 权威树快照不影响续传点；resume 场景（pause→resume、跨进程重启）无碰撞无缝隙（测试 + 事件文件分析）。
- 安全基础面：loopback 硬绑定；token 支持 Bearer/query 且 subtle 常量时间比较；Host 白名单覆盖 SplitHostPort/`[]` 处理与空 Host 拒绝；embed/静态 FS 无法路径穿越；Tauri capabilities 仅 core:default，WebView 不暴露任何命令面；前端树选择按稳定 id 去重、跨快照保留有效选中。

---

## 4. TR-13.2 整体完成度评分

**评分：4 / 5（threshold >= 4，达标）**

评分理由：

1. 后端（RunManager 七态机、事件 seq 体系、leaf 审批桥、SSE 补发/实时）实现完整，全部 Go 测试含 -race 由本人独立复跑通过；
2. 浏览器形态经 S1~S6 六个场景走查，自动/人工审批、abort/adjust、ask、暂停续跑、失败闸门、筛选、日志、文件只读查看主路径均可达且行为正确；dist 可由源码确定性重建；
3. Tauri 形态 dev 与 release（app/dmg、随机端口、关窗无残留）的走查证据与代码一致（未独立重跑，扣分因素之一）；
4. 未给 5 分的原因：存在 1 个 P1（环境 apiKey 在弱模型常见动作下落盘，AC-11 仅 weak）、若干 P2（非 SSE 出站掩码缺失、fd 泄漏、极端时序孤儿进程、补发相位闪回），且 NFR-5 大流量性能无实测。距「双形态主流程全程顺滑、机密口径完全闭合」的 5 分锚点尚有一步。

---

## 5. 结论

**有条件可收尾**：

1. **必须先修复 P1-1**（serve 路径子进程环境清洗，建议同时加落盘前 scrub 与回归测试），使 AC-11 由 weak 转为 pass；这是唯一影响验收结论的问题；
2. 建议同期顺手修复 P2-1（句柄泄漏）、P2-3（出站掩码一致性）、P2-6/P2-7（孤儿进程防御），成本低且直接强化可靠性与安全口径；
3. 其余 P2（P2-2、P2-4、P2-5、P2-8）可记入后续 backlog，不阻断本期收尾；
4. P1-1 修复并由独立测试（含事件/日志 grep 断言）验证后，本特性即满足 AC-1~AC-11 与 TR-13.2 threshold，可正式收尾。
