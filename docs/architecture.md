# 架构与二次开发

本文面向想改 divvy 内部、或想搞清楚"为什么它这样跑"的贡献者。面向使用者的说明在 [README](../README.md)。

## 设计前提

现有编程 Agent 极度依赖旗舰长上下文模型。小模型上常见两类失败：

- **上下文爆炸** — 历史越长，约束被冲掉，幻觉增多。
- **推理过载** — 一次规划过多细节，逻辑崩溃。

divvy 不追求"单次规划很聪明"，而是用工程约束保证复杂目标能被做完：

1. **树状递归拆解** — 复合节点当架构师：划边界、写接口契约、写 Definition of Done。
2. **契约先于执行** — 没有输入/输出约束和可运行验收命令，不下发叶子。
3. **叶子隔离上下文** — 每次执行新建对话；失败重试也是新对话，只附带错误输出。

## 模块与依赖方向

```
cmd/divvy            CLI：flag 解析、模式分发、信号处理
  └─ pkg/agent       编排层：Decomposer / Worker / Verifier / Orchestrator / 预算护栏
       ├─ pkg/engine    任务树、状态机、调度、原子 JSON 持久化
       ├─ pkg/llm       OpenAI 兼容客户端、流式、重试、弱模型 JSON 容错解析
       ├─ pkg/tools     工作区沙箱、镜像、代码/网络/浏览器工具
       ├─ pkg/cost      价目表与 token→美元换算
       └─ pkg/models    纯数据类型（无依赖，可被任意包引用）
```

依赖是单向的：`models` 不依赖任何内部包，`engine`/`tools`/`llm`/`cost` 相互独立，`agent` 负责把它们粘起来，`cmd` 只做参数解析。加新能力时优先放进 `tools`（工具）或 `agent`（策略），避免让 `engine` 依赖 `agent`。

## 一次运行的完整生命周期

```
NewFromGoal / Load
   └─ Orchestrator.Run
        ├─ startBudget            会话级 token / 美元硬上限（含 resume 前的花费）
        ├─ reportPlanWarnings     契约体检（Run 与 -plan 都会跑）
        └─ 主循环
             ├─ GetNextDecomposableNode → decompose      填树
             ├─ GetReadyLeafNodesAvoiding(claims)         取就绪叶子（跳过产出冲突）
             │     └─ executeLeaf
             │          ├─ newLeafMirror                  -isolate：建镜像 + 限定沙箱
             │          ├─ runWorker                      干净上下文 + 工具循环
             │          ├─ verify                         跑 DoD 命令
             │          └─ publishLeaf                    合并镜像 / git commit
             ├─ RootNeedsAcceptance → verifyRootAcceptance 端到端验收
             └─ IsComplete / HasFailed
```

### 状态机

节点状态：`PENDING` → `DECOMPOSING` / `RUNNING` → `VERIFYING` → `COMPLETED` / `FAILED`（另有 `SKIPPED`）。
`checkAndUpdateParent` 在子节点状态变化时**同步**向上冒泡 parent's 聚合状态，通知在树锁之外发出。

注意 `HasFailed()` 只在**所有**子节点都终止后才为真：一个叶子失败而兄弟仍在 `PENDING` 时，父节点是 `RUNNING` 而非 `FAILED`——这是有意的，否则一个早失败的叶子会取消掉还在正常推进的工作。

### 树写入是原子的

`UpdateNode` 让回调在**副本**上执行，只有返回 nil 才替换活节点。回调中途失败不会把半改状态写进会话文件。这是与镜像合并、文件编辑一致的原子性约定：任务树是唯一会被持久化并 resume 的状态源，不能容忍部分写入。

### 叶子为什么看不到别的叶子

叶子 prompt 由 `projectContext` 组装：当前任务、契约、父节点/依赖摘要、至多 4 个契约里点名的输入文件、工作区快照，外加（重试时）上一轮的错误前缀。**不携带**其它叶子的对话历史——这是"愚公移山"策略的落点，也是为什么每个叶子都能塞进小上下文。

## 并发与隔离

`-isolate` 下每个叶子在主工作区的**临时镜像**里工作，验收通过才合并回去，失败即丢弃。合并有两道防线：

1. **调度层（预防）** — `GetReadyLeafNodesAvoiding(claims)` 让声明了相同产出文件的叶子互斥。claim 的判定和占位在**同一次持锁**内完成，因此两个叶子不可能同时拿到同一个文件。产出路径经 `NormalizeOutputPath` 归一（`./a.go`、`a//go`、`a.go` 等价）。
2. **合并层（兜底）** — 契约没写产出（弱模型很常见）时调度层无从判断。`Mirror` 在快照时记录每个文件的 sha256，合并时若发现某文件在本叶子改动之后**又被别人改过**，整批放弃合并并返回 `*MergeConflictError`，绝不覆盖。此时该叶子会重建镜像重试，新镜像已含兄弟叶子的成果，失败重试因此变成一次"对齐"而非死循环。

`mergeMu` 只保证合并串行，不解决语义冲突——语义冲突由上面两层负责。

## 失败与恢复

| 情况 | 行为 |
|---|---|
| 验收失败 | 新建**隔离上下文**重试，注入诊断块（根因前置）+ 错误前缀，指数退避（上限 30s） |
| worker 返回错误 | 同上，另记入节点 `ErrorHistory` |
| 达到 `-max-retries` | 视作"任务太大"，`handleLeafFailure` 把它转成 COMPOUND 重新拆解（受 `-max-redeclare` / `-max-depth` 约束） |
| 连续 `-max-stall` 次**相同**失败 | 判定原地打转，走同一条 `handleLeafFailure`（重新拆解，预算用尽则 FAILED），并记 `leaf_stall` 事件 |
| 超过 `-max-elapsed` | 直接 FAILED 并说明原因；**不**触发重新拆解（时间不够不代表任务太大，重拆只是重置时钟） |
| 用完 `-max-steps` 且未调用 `finish` | 判失败（`ErrMaxSteps`），**不送验收**：模型没声称完成，文件可能半截，`review_diff` 自检也还没跑 |
| 根验收失败 | 回路由给集成叶子，带错误重跑装配 |
| Ctrl+C / 预算超限 | 取消 → 在途节点复位 `PENDING` → 保存任务树，`-resume` 续跑 |

诊断块前置很关键：原始日志有 24KB 上限，而重试 prompt 只截取前 4000 字符，不前置的话最关键的那行会被淹没。

### 停滞检测：为什么必须有第二道闸门

`-max-retries 0`（默认）意味着无限重试，而 `-max-elapsed` 只拦得住**慢**叶子。一个每次都在 2 秒内以同样方式失败的叶子既不慢也不收敛，45 分钟里会重复上千次——时间闸门要等满 45 分钟，token 早烧完了。

所以每次失败都会算一个**指纹**存进 `ErrorRecord.Fingerprint`（`models` 层），`TaskNode.StallRun()` 数尾部连续相同指纹的条数：

- 指纹优先取 `tools.CompactDiagnostics` 抽出的根因行——它们已经去重、已剥离易变细节（耗时、字节数），正是指纹想要的；
- 没有诊断规则命中时回退到"输出前 12 行 + 数字掩码"，这样"3 个用例失败"和"4 个用例失败"仍是同一个失败；
- 空指纹（更早版本写入的历史记录）永不计数，不会误伤 resume 上来的旧会话。

连续 `-max-stall`（默认 3）次相同即停止重试。`-max-stall 0` 关闭检测，退回只有墙钟的旧行为。

## 预算护栏

四层，互相独立：

- `-max-cost` / `-budget-tokens` — 会话级硬上限，计入 resume 之前已持久化的花费；超限立即取消运行并保存。
- `-max-elapsed` — 单个叶子的墙钟预算，跨该叶子的所有尝试累计。管"慢叶子"。
- `-max-stall` — 单个叶子连续相同失败的次数上限。管"快但在原地打转的叶子"。`0` 为不限。
- 节点级 `TokenUsage` — 每次调用的 token 按节点记账并随树持久化，resume 后自动累加。

## 会话事后分析

`-log`（原始文本）与 `-events`（JSONL，便于 `jq`）覆盖细粒度回溯；`-report` 是二者的**汇总视图**：目标、进度、token 与估算成本、事件计数（llm/工具/验收失败/重试/停滞/重拆/放弃/超时）、任务树、逐节点的 token 与成本表，以及失败节点的最后错误。它只读持久化状态，因此对失败或被中断的会话同样可用——那正是需要它的时候。

事件汇总由 `agent.SummarizeEvents` 完成（无法解析的行只计数、不中断解析）；`-prune -keep N` 清理旧会话（含其日志与事件流），`LATEST` 指向的会话永不删，`-dry-run` 可先看清单。

## 扩展点

### 加一个工具

1. 在 `pkg/tools` 实现，签名挂在 `Sandbox` 上（`Sandbox` 已有 `Web` / `Browser` 之类的可选能力字段）。
2. 在 `pkg/tools/call.go` 的 `Call` 分发表里注册名字。
3. 在 `NativeXxx()` 里补一份原生 `tool_calls` 声明（仅 `-native-tools` 用）。
4. 在 `Sandbox.DynamicToolDescriptions` 里加一行人类可读描述——**弱模型主要靠这段文字认识工具**。
5. 若工具只读，记得在 `previewArgs`（`agent` 包）里加一行，否则运行日志里看不到它在操作什么。

### 改 prompt

`pkg/agent/prompts.go` 是唯一的 prompt 来源。`workerSystemFor` 会把动态工具描述拼进去。弱模型对 prompt 极其敏感：改完务必用 `-plan` 先看拆解粒度是否还合适。

### 加验收维度

`pkg/tools/project.go` 负责识别技术栈并生成默认 DoD；`pkg/tools/diagnostics.go` 负责从失败输出里抽根因。新增语言时这两处要一起改，否则会出现"能验收但报错看不懂"或反之。

诊断规则是**声明式**的：每个工具链对应 `diagRules` 里的一个 `diagRule`（`anchored` 匹配带位置的错误行，`status` 匹配判定/异常类别行），加语言是加一条数据而不是改控制流。`diagRuleFor` 必须对 `DetectProject` 可能返回的每个 `ProjectType` 都有规则——否则该工具链的失败会**完全不压缩**，把 24KB 原始日志整个塞进重试 prompt。`TestDiagRuleForCoversEveryProjectType` 就是守这条不变量的。目前 Go/Python/Rust/Node 各有专用规则，Makefile 与通用工作区有兜底规则。

## 测试

```bash
go test ./...            # 全量，端到端用脚本化 Mock LLM，不联网
go test -race ./...      # 并发路径（-parallel / -isolate / 浏览器租约）
golangci-lint run ./...  # 已配置 .golangci.yml
```

端到端测试会在临时目录里真正 `go test` 验收生成的包，所以能抓到"树全绿但产物跑不起来"这类问题。写并发相关测试时优先断言**可观察的不变量**（例如"冲突叶子不得重叠执行"），而不是断言某段日志——否则重构会频繁误报。

写 `cmd` 层测试时注意：**不要触发真实 LLM 请求**。`llm.OpenAIClient` 对可重试错误默认无限重试，打到一个不存在的 endpoint 会让测试永久挂起（而不是快速失败）。要用网络路径就用 `httptest` 注入；否则直接写出持久化产物（如 `.divvy/<id>.json`）再跑只读模式。

CI 有四个 job：常规测试（含 gofmt 检查）、`-race`、`golangci-lint`、以及装了 Chrome 的浏览器 job（本地无浏览器时相关用例会跳过）。
