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
        ├─ baselineAggregate      叶子跑之前先量一次工作区（wave.go）
        ├─ reportPlanWarnings     契约体检（Run 与 -plan 都会跑）
        └─ 主循环
             ├─ waveBoundary → runWaveAggregate     整波落地 → 整仓构建/测试
             │     └─ 失败 → ensureRepairLeaf       聚合失败的活成为下一波第一个任务
             ├─ GetNextDecomposableNode → decompose  填树（受 waveHasRoom 节流）
             ├─ GetReadyLeafNodesAvoiding(claims)     取就绪叶子（跳过产出冲突，每波最多发起 -wave 个）
             │     └─ executeLeaf
             │          ├─ newLeafMirror                  -isolate：建镜像 + 限定沙箱
             │          ├─ runWorker                      干净上下文 + 工具循环
             │          ├─ verify                         跑 DoD 命令
             │          └─ publishLeaf                    合并镜像 / git commit
             ├─ RootNeedsAcceptance → verifyRootAcceptance 端到端验收
             └─ IsComplete / HasFailed
```

### 波次循环：拆分 → 执行 → 聚合 → 再拆分

主循环不是"开局拆完、最后装一次"，而是一圈圈转。实现在 `pkg/agent/wave.go`：

| 环节 | 函数 | 判据 |
|---|---|---|
| 节流（规划） | `waveHasRoom` | 已规划未执行的叶子数 < `-wave`；能跑的叶子为空时强制放行（否则死锁）；有拆解在途时一律不放行 |
| 节流（执行） | `waveBudget` | 一波最多发起 `-wave` 个叶子；跑满即进入波边界 |
| 波边界 | `waveBoundary` | 配额跑满或计划榨干、没有 in-flight、根未完成也未失败 |
| 聚合 | `runWaveAggregate` | 跑 `-aggregate` 或按工具链推断的整仓命令 |
| 修复 | `ensureRepairLeaf` | 聚合失败的输出变成下一波第一个叶子（无产出、无依赖，立即可跑） |

五个刻意的选择：

1. **配额同时管规划和执行。** 只节流拆解是不够的——真跑时根节点一次吐出全部 6 个叶子，第一个聚合同样落在所有叶子之后。弱模型最常做的是"把目标一把拆成所有叶子"，所以配额必须同时是"一手能规划几个"和"一手能跑几个"。
2. **聚合先于根验收。** 验收是昂贵且不对人的端到端断言；工作区根本构建不过时跑它只是浪费，而聚合失败有明确责任人（修复叶子）。顺序在 `orchestrator.go` 主循环里，`waveBoundary` 分支在 `RootNeedsAcceptance` 之前。
3. **一次只放一个拆解在途。** 拆解是异步的，`PendingLeafCount` 在它返回前仍是旧边界，若不拦住就会超额放行一整棵子树。`Orchestrator.planning` 计数即为此。
4. **基线只对"已有源码"的工作区成立。** 空工作区里 `go test ./...` 必然失败（"no packages to test"），若记成基线，之后每次失败都会被判成"历史问题"而永不修复——恰好废掉这个机制。同样的理由让**派生**的聚合命令在没有任何源码时直接跳过；显式 `-aggregate` 是操作者的契约，不受此限。
5. **`-wave 0` 是诚实对照组。** 关闭节流、配额、聚合、修复叶子，回到旧的平坦行为，用来对比同一目标在两种模式下的叶子数、token 与"最后能不能跑起来"。

节流是**近似**的：判据在拆解之前算，一次拆解可能吐出多于剩余额度的叶子，`PendingLeafCount` 因此可以超过 `-wave`。这是有意的——精确配额会把一次拆解切碎，而拆解的粒度本来就是模型给的。执行侧则有硬配额，超出的叶子留到下一波。

### 状态机

节点状态：`PENDING` → `DECOMPOSING` / `RUNNING` → `VERIFYING` → `COMPLETED` / `FAILED`（另有 `SKIPPED`）。
`checkAndUpdateParent` 在子节点状态变化时**同步**向上冒泡 parent's 聚合状态，通知在树锁之外发出。

注意 `HasFailed()` 只在**所有**子节点都终止后才为真：一个叶子失败而兄弟仍在 `PENDING` 时，父节点是 `RUNNING` 而非 `FAILED`——这是有意的，否则一个早失败的叶子会取消掉还在正常推进的工作。

### 树写入是原子的

`UpdateNode` 让回调在**副本**上执行，只有返回 nil 才替换活节点。回调中途失败不会把半改状态写进会话文件。这是与镜像合并、文件编辑一致的原子性约定：任务树是唯一会被持久化并 resume 的状态源，不能容忍部分写入。

### 叶子为什么看不到别的叶子

叶子 prompt 由 `projectContext` 组装：当前任务、契约、父节点/依赖摘要、至多 4 个契约里点名的输入文件、工作区快照，外加（重试时）上一轮的错误前缀。**不携带**其它叶子的对话历史——这是"愚公移山"策略的落点，也是为什么每个叶子都能塞进小上下文。

## 并发与隔离

`-isolate` 下每个叶子在主工作区的**临时镜像**里工作，验收通过才合并回去，失败即丢弃。合并有三道防线：

1. **调度层（预防）** — `GetReadyLeafNodesAvoiding(claims)` 让声明了相同产出文件的叶子互斥。claim 的判定和占位在**同一次持锁**内完成，因此两个叶子不可能同时拿到同一个文件。产出路径经 `NormalizeOutputPath` 归一（`./a.go`、`a//go`、`a.go` 等价）。
2. **合并层（兜底）** — 契约没写产出（弱模型很常见）时调度层无从判断。`Mirror` 在快照时记录每个文件的 sha256，合并时若发现某文件在本叶子改动之后**又被别人改过**，整批放弃合并并返回 `*MergeConflictError`，绝不覆盖。此时该叶子会重建镜像重试，新镜像已含兄弟叶子的成果，失败重试因此变成一次"对齐"而非死循环。
3. **布局层（前置）** — 符号链接让同一份文件拥有两个路径。副本镜像不携带链接（`copyTree` 跳过非常规文件），叶子在镜像里建 `core/x.go` 与另一叶子在 `internal/core/x.go` 上工作，在合并看来是两个不同文件；而合并若穿过链接，写入落到链接指向处而非待合并路径。启动时 `tools.CheckLayout` 只读目录项（不读源码、不跟随链接）报出符号链接与"模块路径 / 目录层级差一层"，合并阶段再用 `*MirrorLayoutError` 拦住穿越链接的写入。二者刻意分开：`MirrorLayoutError` **不是**并发冲突，重新快照得到同样的结果，重试只会空转。

叶子在 verify 通过、合并之前还会跑 `tools.CheckLeafLayout`：模块路径与目录差一层、多余的 go.mod、契约产出落到模块同名目录下，都会以 `layout_drift` 让该叶子失败并给出 found vs expected 路径，避免 soft grep DoD 放行后让后续叶子烧 token 修 import。实现叶子的 DoD 应优先 compile/test；docs/test-only 叶子才允许只用 grep/`test -f`。弱模型连续空/非法 JSON 超过阈值会提前中止该 attempt，不再把 MaxSteps 烧完。

`mergeMu` 只保证合并串行，不解决语义冲突——语义冲突由上面三层负责。

### 执行后端：没有沙箱就不跑命令

`run_bash` 的执行路径按平台分文件（`pkg/tools/sandbox_isolate_{linux,darwin,other}.go`），共享的只有两样东西：`isolatedEnv`（剥掉继承环境，只留 PATH/HOME/TMPDIR 等，见 `sandbox_isolate_env.go`）和 `ProbeShellIsolation`。

| 平台 | 后端 | 手段 |
|---|---|---|
| Linux | bubblewrap（`bwrap`） | 空网络命名空间、`/` 只读挂载、工作区与临时目录可写 |
| macOS | `sandbox-exec` + SBPL profile | `(deny default)` 后逐条 allow；家目录不 allow 即不可读；`(deny network*)`；按名字 deny `com.apple.securityd` / `trustd` |
| Windows | 无 | 直接返回 `ErrExecutionUnavailable` |

三条不变量对所有后端都成立：

1. **不可用就报错，不退回裸 shell。** 沙箱建不起来（`sandbox-exec` 不在、profile 生成失败、`DIVVY_EXEC_BACKEND=none`）时返回 `ErrExecutionUnavailable` 且**命令根本没启动**。这是 `ApplyRuntimePolicy` 的存在意义：宁可让 agent 瞎着，也不让它无隔离地跑模型写的命令。
2. **启动时实测，不信假设。** 两个平台的探针都真跑一次命令（`ProbeShellIsolation`）：Linux 确认 `bwrap` 在并且能跑通 `true`；macOS 额外验证两条**否定性**断言——往工作区外写被拒、家目录里的哨兵文件读不到。判定靠 `os.Stat` 和退出码，不抓输出（输出格式跨版本不稳）。探针不过界面上就显示"不可用"，而不是运行到一半才发现。
3. **临时目录与 HOME 是重定向的。** 每次执行都用 `$TMPDIR/divvy-exec` 下的私有 `home`/`gopath`，`HOME` 指过去，所以 `ls ~` 看到的不是真实家目录。Go 的 build cache 单独放在 `divvy-gocache` 并跨次复用，否则每次 `go build` 都从零编译。

macOS 后端有两个只能靠实测发现的坑，值得记下来：

- **Seatbelt 按调用者拼写的路径匹配。** `os.TempDir()` 在 macOS 返回 `/var/folders/...`，内核看到的是 `/private/var/folders/...`；`/var` 在 SBPL 里写 `(subpath "/var/...")` **不会**匹配 `/private/var/...`。所以 profile 里每条路径都要同时写 literal 与 `realPath` 两种拼写，并且传给子进程的 `TMPDIR`/`GOCACHE` 必须是解析后的拼写——否则 `go build` 会以 `mkdir ... operation not permitted` 失败，而 profile 看上去"明明允许了"。
- **`sandbox-exec` 是 deprecated 但仍可用。** Apple 从 10.15 起在文档里弃用它，实测到 macOS 26/27 仍工作。所以探针是唯一可信的判据：将来它被移除，探针失败、`run_bash` 退回不可用，引擎行为不会改变。

SBPL 是权限黑名单式封锁，不是命名空间隔离：`/usr`、`/Library` 这类系统前缀在沙箱内可读，可写面仍只有工作区与临时目录，网络出口被 deny。够用来跑构建和测试，不等价于"只读整个磁盘"——家目录的私有文件靠"不 allow"挡住，而不是靠"看不见"。

### 拒绝写的时候要说明白（`sandbox_hint.go`）

沙箱拒绝一次写，内核给的原文是 `mkdir: /tmp/app: operation not permitted`。这行字对模型来说是**误导性的**：它读起来像项目里的 bug，而不是一条策略。实测过一次完整代价——`go build -o /tmp/app` 被拒后，模型去查 git（怀疑 VCS stamping）、试 `xcode-select`、最后 `sudo`，十步之后开始吐截断的 JSON。它一直在对抗的正是那个沙箱，而没有任何东西告诉它"换个目录就行"。

所以拒绝写这件事要在两个层面各说一次：

- **事前**：叶子提示词与直接模式的系统提示都写明"临时文件放工作区或 `$TMPDIR`，裸 `/tmp` 可能不成立"。能预防就不靠事后解释；`run_bash` 不可用的宿主上这句不出现（那里根本没有命令会失败到这个原因）。
- **事后**：`run_bash` 与验收命令的输出末尾追加一行 `[sandbox]`，指出可写的位置和改法。两处共用 `tools.SandboxDenialHint`，因为它判断的是同一件事，分开写迟早会漂移。

判定只认两种无歧义标记：`Operation not permitted`（seatbelt / EPERM）和 `Read-only file system`（bwrap 的 `--ro-bind`）。**`Permission denied`（EACCES）故意不认**——项目自己也会产生它（`chmod`、读不了的 fixture），在那儿加沙箱建议等于把模型引向错误的排查方向，和"把应该成立写成已经验证"是同一类错误。这条边界有测试守着（`TestSandboxDenialHint_Markers`），集成侧另有 `TestRunBash_ExplainsSandboxWriteRefusal` / `TestVerify_SandboxWriteRefusalExplainsItself` 证明它真的接在两条输出路径上。

注意这**不是**给沙箱开 `/tmp`。策略一个字没改：可写的仍然只有工作区与重定向后的 `$TMPDIR`（macOS 上是 `$TMPDIR/divvy-exec`，Linux 上是每次执行新挂的 tmpfs）。改的是"失败能不能被读懂"。

## 失败与恢复

| 情况 | 行为 |
|---|---|
| 验收失败 | 新建**隔离上下文**重试，注入诊断块（根因前置）+ 错误前缀，指数退避（上限 30s） |
| worker 返回错误 | 同上，另记入节点 `ErrorHistory` |
| 达到 `-max-retries` | `handleLeafFailure`：先诊断拆解/DoD 是否有问题，再转 COMPOUND 重新拆解（受 `-max-redecompose` / `-max-depth` 约束） |
| 连续 `-max-stall` 次**相同**失败 | 判定原地打转，走同一条 `handleLeafFailure`，并记 `leaf_stall` 事件。若诊断为 DoD 缺陷（如 `expected_output` 与命令输出对不上），即使已到 `-max-depth` 也会做**原地修计划**（强制 atomic 重写 contract/DoD）。重拆预算用尽后叶子回到 `PENDING` 继续重试，不标 `FAILED`，避免整棵树无任务可跑而退出 |
| 超过 `-max-elapsed` | 直接 FAILED 并说明原因；**不**触发重新拆解（时间不够不代表任务太大，重拆只是重置时钟） |
| 用完 `-max-steps` 且未调用 `finish` | 判失败（`ErrMaxSteps`），**不送验收**：模型没声称完成，文件可能半截，`review_diff` 自检也还没跑 |
| 计划结构不完整 | `planBlockers` 在规划期发现根验收命令**运行**的路径无人产出、或**读取**的数据文件无人创建 → 回退重拆（受 `-max-redecompose`）；预算用尽则自动注入 `integrate_entry` 叶子（它同时接管缺失的输入文件），不让根带着不可修复的计划开始执行 |
| 根验收失败 | 回路由给集成叶子，带错误重跑装配。错误里必须带**失败的那条命令和输出**，并明说"你自己的 DoD 过了不算数"——叶子原有的 DoD 往往比根验收窄，模型只看见自己那个绿勾就无事可做 |
| `-resume` 且根已 FAILED、缺集成叶子 | 启动时 `healPlanStructure` 补叶子并把根置回 `RUNNING`，避免退化成无限空转 |
| Ctrl+C / 预算超限 | 取消 → 在途节点复位 `PENDING` → 保存任务树，`-resume` 续跑 |

诊断块前置很关键：原始日志有 24KB 上限，而重试 prompt 只截取前 4000 字符，不前置的话最关键的那行会被淹没。

### 根验收命令是一条链，不是一袋词（`pkg/agent/acceptcmd.go`）

根验收命令和任务树是两次独立的模型调用产出的，中间没有任何东西比对它们。`planBlockers` 补的就是这个缺口，但它只能问对问题。

问法错了比不问更糟。早期的实现把整条命令按空格切开当一袋词，于是 `go build -o /tmp/app . && /tmp/app test.json` 里的 **`&&` 被当成了一个必须交付的路径**——一个谁也造不出来的 blocker。它会烧掉 `-max-redecompose` 预算（默认 2 次重拆 + 1 次初始调用 = 3 次规划器调用），逼着规划器去"修"一个不存在的问题；预算用尽后还会注入一个集成叶子，把一次本该顺利的运行搅成一次重试风暴。

所以验收命令按 **shell 链**读：先按 `&&` / `||` / `;` / `|` 切成简单命令，再对每一段问三个问题：

- **它自己产出什么**：`-o` 的目标、重定向 `>` 的目标、`go build ./cmd/x` 隐式生成的 `x`、`cp`/`mv` 的终点。链自己产出的东西不算交付物——`./app` 是上一段 `go build -o app` 造的，不是某个叶子该声明的输出。
- **它需要什么存在**：只有明确指向可执行文件/包路径/源文件的形态才算（`go run ./cmd/demo`、`./bin/app`、`python3 app.py`）。包级范围（`go test ./...`）不算，因为它指不出"哪一个文件缺失"。
- **它读了什么数据文件**：只认数据/配置类扩展名（json/yaml/toml/csv…）。源文件是产出侧的事；工具自己生成的报告（junit xml、coverage json）不该被当成"必须预先存在"。

工作区外的路径（`/tmp/app`）不算交付物：没有叶子能拥有它，把它列进 blocker 等于给出一个无解的死局。

"读取"这一侧是新加的。真实的一次运行里，根验收写的是 `... && /tmp/app test.json`，而 `test.json` 既没有叶子产出、工作区里也没有——**工作可以做到完美，验收照样过不了**。现在这种情况在规划期就被拦下来，并且明确告诉规划器两条出路：加一个产出它的子任务，或者把它从命令里拿掉。预算用尽走集成叶子兜底时，那片叶子会同时接管缺失的输入文件，否则这个缺口会在每次 resume 时重新冒头、再长出一片重复的装配叶子。

> 反向证伪的做法：把 `splitCommandChain` 改回"整条命令一袋词"，8 个用例立刻报红，并且报红的形态与线上 bug 一模一样（`goal acceptance references "&&" …`）。没有这一步，"全绿"可能只是没测到东西。

### 停滞检测：为什么必须有第二道闸门

`-max-retries 0`（默认）意味着无限重试，而 `-max-elapsed` 只拦得住**慢**叶子。一个每次都在 2 秒内以同样方式失败的叶子既不慢也不收敛，45 分钟里会重复上千次——时间闸门要等满 45 分钟，token 早烧完了。

所以每次失败都会算一个**指纹**存进 `ErrorRecord.Fingerprint`（`models` 层），`TaskNode.StallRun()` 数尾部连续相同指纹的条数：

- 指纹优先取 `tools.CompactDiagnostics` 抽出的根因行——它们已经去重、已剥离易变细节（耗时、字节数），正是指纹想要的；
- 没有诊断规则命中时回退到"输出前 12 行 + 数字掩码"，这样"3 个用例失败"和"4 个用例失败"仍是同一个失败；
- 空指纹（更早版本写入的历史记录）永不计数，不会误伤 resume 上来的旧会话。

连续 `-max-stall`（默认 3）次相同即停止重试。`-max-stall 0` 关闭检测，退回只有墙钟的旧行为。

### 单上下文模式（`-solo`）里的同一套闸门

`-solo` 没有叶子，但两个问题一模一样，所以复用的是同一组机制而不是新写一套：

- **停滞**：`soloSession.noteFailure` 用同一个 `stallFingerprint` 判连续相同失败。区别在处置：叶子会被重拆，solo 没有可拆的东西，所以第 2 次相同失败时换的是**提示词**——要求先写清假设、加打印或最小复现去观测、观测推翻假设就换假设；到 `-max-stall` 次直接 `break`，报 `stalled` 并写明"提前收手"的原因，不把剩余轮数耗完。改验收命令会重置计数（不同命令的指纹不可比）。
- **轮内停滞**：只在轮边界判是不够的。一轮有 60 步，模型可以在边界之前就把步数耗在"改一处-跑一次"的循环里——实测第 38 步仍在同一断言上打转，失败数值在 `-0.15` 与 `-1953` 之间来回，说明每次修改都是新猜想而非观测。所以 `trackStepFailure` 盯 `run_bash` 的失败输出：同一条命令连续两次以相同输出失败就立刻往对话里插一条干预（每轮最多 2 次），要求先观测再改代码，并禁止削弱断言。
- **验收漂移**：`-solo` 开始时工作区往往是空的，`tools.DetectProject` 只能给出占位验收（"有文件就算过"），通过它证明不了任何事。所以自动验收标记为 `autoAcceptance`，在每轮开始和验收前各重新解析一次（`go.mod` 出现即升级为 `go test ./...`），并把变更告知模型。`-dod` 给出的验收是契约：不重新解析，直接覆盖规划器与工具链的推断。
- **收尾审计**：验收命令能证明的只是它检查的那些事。实测有一次 `go vet`/`go test`/`go run` 全过，而目标要求的"向量运算用表驱动测试"没有（没有 `t.Run`）、"每 0.2 秒打印一次模拟 2 秒"的 demo 实际打印 120 次跑 24 秒——两条都是目标明写、命令表达不了的。所以验收首次通过后再花一次调用，让模型逐条对照目标要证据，对不上就现在做。审计只做一次，且有单独的一轮预算，不会因为审计把已通过的运行拖死。

这两点是实测出来的，不是设计时想到的：第一次 solo 实跑"通过"了 `ls`，模型自己在对话里承认测试还在失败；第二次实跑同一处代码改了两遍，穿透深度从 `0.0985` 恶化到 `-0.3973`。

### solo 的状态放在对话里

树模式的进度就是节点状态，`LoadTree` 读回来即可续跑。solo 的进度是对话本身——它写过哪些文件、为什么那么改、试过什么——这些都不在树里。所以：

- 转录按 JSONL 逐条落盘到 `<datadir>/<session>.solo.jsonl`，每 10 步与每轮结束重写一次（临时文件 + rename，读者只会看到完整的旧版本或完整的新版本）。读取容忍被截断的最后一行：崩溃的代价是一条消息，不是整段记忆。
- `-resume` 时保留历史、刷新 system prompt（工具文档与规则可能已经变了）、再追加一条"当前工作区长这样"的用户消息。首轮那份快照已经过期，不重述的话模型会对着已经不存在的文件做计划。
- 已完成的 solo 会话（根 `COMPLETED` 且 `IntegrationVerified`）再 `-resume` 是空操作。树是单个叶子，没有别的记录说明这个目标已经做完了，重跑等于把通过验收的交付物重做一遍。

同一条`RunSolo` 也负责第一次进入非空工作区的情况：提示词会说明"这里不是空白开始，先跑验收命令，读文件再替换它"。实测把原始目标喂给一个已经有可用实现的工作区时，模型按目标里"从零实现"的字面意思把所有文件重写了一遍。

## 预算护栏

四层，互相独立：

- `-max-cost` / `-budget-tokens` — 会话级硬上限，计入 resume 之前已持久化的花费；超限立即取消运行并保存。
- `-max-elapsed` — 单个叶子的墙钟预算，跨该叶子的所有尝试累计。管"慢叶子"。
- `-max-stall` — 单个叶子连续相同失败的次数上限。管"快但在原地打转的叶子"。`0` 为不限。
- 节点级 `TokenUsage` — 每次调用的 token 按节点记账并随树持久化，resume 后自动累加。

## 会话事后分析

`-log`（原始文本）与 `-events`（JSONL，便于 `jq`）覆盖细粒度回溯；`-report` 是二者的**汇总视图**：目标、进度、token 与估算成本、事件计数（llm/工具/验收失败/重试/停滞/重拆/放弃/超时，以及波次循环的聚合次数/失败次数/修复叶子数）、任务树、逐节点的 token 与成本表，以及失败节点的最后错误。它只读持久化状态，因此对失败或被中断的会话同样可用——那正是需要它的时候。

聚合一行只在循环真的跑过时才出现（`-wave 0` 的会话不会挂一排零）：通过的次数和失败的次数分开记，否则"聚合了 5 次"既可能是全程绿灯，也可能是坏了 4 次。

事件汇总由 `agent.SummarizeEvents` 完成（无法解析的行只计数、不中断解析）；`-prune -keep N` 清理旧会话（含其日志与事件流），`LATEST` 指向的会话永不删，`-dry-run` 可先看清单。

## 联网抓取的地址策略

`-web` 下的 `web_fetch` / `http_request` / `download_file` 遵守同一条策略：只允许 http/https；只允许 80/443；目标解析后不得是回环/私网/链路本地/CGNAT/保留段（`tools.isPublicIP` 显式覆盖 Go `net.IP` 谓词漏掉的非全局可路由范围：NAT64 `64:ff9b::/96`、TEST-NET、`240.0.0.0/4`、广播地址等）；每一跳重定向都重新校验。

**不变量：策略在 dial 时强制执行。** transport 的 `DialContext`（`policyTransport`）在建立连接的瞬间重新解析、校验、并直连其中之一是校验过的 IP。只在请求前校验 URL 不够——重绑定 DNS 可以让校验时看到公网 IP、实际连接时解析到内网 IP（TOCTOU）。检查放进 dialer 后，"通过校验的地址就是被连接的地址"对初始请求和全部重定向同时成立。`validateURL` 仍然保留在请求前，只为 fail-fast 与清晰错误；真正的保证在 dialer。`-search-url` 的检索端点由操作者显式配置，与 `AllowLocal` 同级信任（自建 SearXNG 常在内网），使用独立的 `searchClient`，不走该策略。

## 弱模型基准

`cmd/bench`（`divvy-bench`）把"引擎在弱模型上真的能用"变成可重复度量：固定的小目标语料库 × 真实 OpenAI-compatible 端点（默认本机 Ollama），每目标在全新工作区跑完整流程并报告成功率/token/耗时/引擎失败计数器。语料、指标与参考数字见 [weak-model-bench.md](weak-model-bench.md)。

`-loop` 决定测哪个循环。树模式的成败取自计划里的 DoD（引擎自己跑验收）；直接模式没有契约，所以 harness 在 `NewDirect(o, nil).Run(ctx)` 返回后自己跑 `Goal.Accept`——语料里每个目标都必须声明它，`TestBench_CorpusDeclaresAcceptance` 守住这一点，否则直接模式就只能靠模型自述打分。宿主没有执行能力时验收标 `skip` 并附原因，那一行不算"已验证"。两条路都设计成不要求 shell 即可跑通 harness 本身（`TestBench_DirectLoopDrivesGoal` 用脚本模型 + 两次 `write_file` + `finish`），这样才能在 Linux CI 上既测 harness 又测验收。

## 编辑锚点的容忍度（`pkg/tools/anchor.go`）

`replace_lines` 的 `old_string` 是**模型抄来的文本**，不是编辑器选中的区间。小模型稳定地在三处出错：把 `read_file` 的 `  12: ` 行号一起抄进去、把缩进改掉（tab↔空格、多一级）、丢掉编辑器保存时清掉的行尾空白。字节精确匹配会把这些全部变成 `old_string not found`，而弱模型对 not found 的反应通常是**原样再发一次**——正是这个项目要消灭的循环。

所以定位分三趟：精确 → 剥掉行号前缀后再精确 → 去掉全部水平空白（空格/tab/CR，保留换行）后按投影位置反查原文件偏移。第三趟得到的区间是**原文件的真实字节区间**（含它自己的缩进），替换写进去的是原文那一块，不是模型那份重新缩进过的副本。

容忍度只影响"能不能找到"，不影响"会不会改错"：任何一趟命中之后仍然要求**唯一定位**，多命中照旧报错，并且错误里逐个列出命中行号。真正的兜底在失败路径上——匹配不上时错误会引用文件里最接近的一段（按行投影相似度取窗口，至少 1/4 行命中才给提示），带真实行号和真实原文，模型照着改锚点就行。

两个容易踩的实现细节：`trimBlankEdges` 只裁剪**多余的空行**，必须保留末尾那一个换行——否则替换后文件自带的换行会和替换文本末尾的换行叠成空行；CRLF 文件先在 LF 视图中编辑再整体还原行尾，锚点也做同样的对齐，避免一次编辑翻新整份文件的行尾。

### 陈旧读取闸门（`pkg/tools/stale.go`）

锚点门只回答一个问题："这段文本是否**恰好出现一次**"，它从不回答"这是不是你刚才读的那个文件"。而这两件事在真实会话里会分开：用户在你思考时保存了文件、`run_bash` 里的 gofmt 重写了它、代码生成器重新生成它。只要那段文本还在（挪了位置、出现在重复的辅助函数里、文件末尾多了一段），编辑就会**成功**并落到错误的位置——而且照样能编译，下游检查一句都不会说。

所以 sandbox 记住模型观察过的每个文件的 sha256：`read_file`（含行区间读、截断读，摘要按整文件算）、自己 `write_file` 的内容、以及自己每次成功编辑后的结果。下一次 patch（`replace_lines` 的三种形态）先比对当前内容摘要，不一致就拒绝，文件一个字节都不动，并告诉模型"读一遍再改"——这正是人类会做的恢复动作。

四条刻意的边界：

1. **只闸 patch，不闸整文覆盖。** `write_file` 声明了完整结果，没有"锚点放错位置"这回事。
2. **没观察过的文件放行。** 树模式的叶子、resume 后的会话都会改"这个 sandbox 里没见过"的文件；把闸门变成无法自证的墙，比漏掉一次更糟。
3. **超 32MiB 不追踪。** 那种文件 `read_file` 本来就截断，每次读都整份哈希不值；不追踪意味着放行，不是发明一个拒绝。
4. **记录按工作区相对路径存。** 所以手动审批模式换到镜像副本后依然有效，也能随转录一起持久化到下一轮（`directState.Observed`）——跨轮才是它真正值钱的场景：用户在你空闲时改了文件，下一轮你用旧锚点去改，锚点还在，位置已经不是那个位置了。

`ObservedFiles` / `RestoreObserved` 是这层的持久化接口：前者返回副本（调用方改它不影响 sandbox），后者只做**播种**——已有的记录优先，恢复不能把更新的一次观察回滚掉。

## 直接模式的"观察"闭环（`pkg/tools/postedit.go` + `direct.go`）

工具循环的四个动作里，**Observe 最容易被模型偷掉**：写完文件不跑构建，循环就退化成"改完就说完成"。树模式不存在这个问题（每个叶子有真实验收命令），直接模式原先只有一句提示词要求模型自己跑验证——实测过：写完 `func Broken( {` 再 `finish`，引擎返回 `run error=<nil>`，破损工作区被当作成功回合。

现在编辑类工具返回后引擎自动补一步观察：

1. `editedPaths` 取出本次改动的文件（单个、移动的两端、删除也算——删掉一个文件同样可能让包不编译）。
2. `planEditCheck` 按**工作区根部的标记文件**选工具链（不是按文件后缀猜），再选最便宜的命令：Go 非测试文件 `go build ./<包>`、`_test.go` 用 `go vet ./<包>`（测试文件只在 test 构建里编译）；Python 用 `ast.parse`（`py_compile` 会往用户工作区写 `__pycache__`，污染 turn diff）；Node 单文件 `node --check`；Rust `cargo check`。最多 3 条命令，默认 20s，避免"改一处、全仓构建"。
3. 结果以 `[auto-verify]` 追加在**同一条工具输出**里。失败时：同一回复里后续动作不执行（它们是在模型看到错误之前就计划好的），并把命令记进 `recheckCmds`。
4. `finish` 前重跑 `recheckCmds`。重跑而不是信一个布尔位：修在别的文件里的错误也能清掉，仍然坏的工作区则不能宣称完成。

三条边界：宿主没有可用的执行后端（Windows，或 macOS 上 `sandbox-exec` 不可用）时整段跳过并记日志，**不**降级成无沙箱执行；识别不出工具链时给出一行 `skipped: ...` 而不是假装验证过；检查本身出错按"跳过"处理，不让工具的故障卡死交付。另外，同一失败连续拒绝 `finish` 两次后闸门放行并记 `edit_check_waived` 事件——否则"工具链本身坏了"（模块路径不对、要联网下载依赖）会变成一个永远结束不了的回合；诊断已经留在转录里，用户看得到。

### 没有 shell 时的进程内兜底

**Linux 和 macOS 都有后端，Windows 没有**，`run_bash` 不可用时上面那套构建命令一条也跑不了。这不是可以靠"降级"绕过的：`ApplyRuntimePolicy` 明确不提供无沙箱执行。结果是 Windows 上的直接模式曾经（macOS 也是，在 `sandbox-exec` 后端之前）**完全没有观察**——改了代码，没有任何东西会说话。

`go/parser` 补上了这一层，代价为零个进程：编辑后的 `.go` 文件在进程内解析，错误同样带 `file:line:col`，同样进 `[auto-verify]`、同样记进 `recheckCmds` 让 `finish` 被拒。它是**兜底**而不是替代——有后端时仍然跑真构建，只有拿不到 shell 才降级到解析。

设计上有三个刻意的选择：

1. **标签不是 `go build`**。内置检查在命令列表里写作 `go syntax <file>`，`EditCheck.SyntaxOnly()` 据此把通过时的措辞改成「语法通过，本环境无法构建，类型与测试仍未知」。让模型把"能解析"读成"构建通过"，比不给反馈更糟。
2. **提示词同步认账**。`run_bash` 不可用时系统提示直接写明：本环境不能执行命令，`[auto-verify]` 只是语法解析，**不许声称构建或测试通过**。
3. **列表仍是 `[]string`**。finish 闸门把"要重跑什么"存成字符串列表并原样重放，所以内置检查也必须能穿过这个往返；`go syntax` 不是真实的 go 子命令，万一拦截失效也只会报错，不会执行到别的东西。

其它语言没有可用的进程内解析器（Python/Node/Rust 都要解释器或编译器，也就是我们没有的 shell），仍然只记一行 skip——声称验证过比不验证更有害。

## 编辑器内 AI 改写（`pkg/server/ai_edit*.go`）

一句话：**让模型只写代码，让服务端写 diff。**

弱模型输出结构化 JSON 的失败率远高于输出代码。所以 `/ai-edit` 不要求模型描述自己改了什么，只要求它返回「选中行范围的替换代码」；服务端拿返回值和发出去的选区做 `computeDiffLines`（与审阅界面同一套 diff），把结果切成 hunk 交给 UI 逐块接受/拒绝。JSON 解析失败这条失败路径被整体删掉了。

三条不变量：

1. **选区由服务端算。** 请求只带 `start_line`/`end_line`（或 `insert_line`），选区原文一律从磁盘文件按行偏移切出来，客户端传来的 `text` 不参与计算——否则一次过期的选区就会改到别的地方。
2. **写回前双重校验。** `base_hash`（整文件 sha256）和 `old_text`（选区原文）任一不匹配就 409，一个字节都不写。这覆盖"生成期间文件被外部改过"和"客户端行号过期"两种真实情况，而不是把改动静默合并进别人的修改里。
3. **撤销独立于轮次日志。** 轮次回滚日志按 session 消费，AI 改写没有轮次；写进去会在下一次任意轮次撤销时被当作"已撤销"。所以用单独的单槽记录（`conversations/<id>.ai-edit-undo.json`），撤销时还要求文件内容仍等于这次写回的产物，否则拒绝覆盖。

流式（`stream: true`）用 SSE 发 `delta` / `done` / `error`，`delta` 就是模型原始代码，`done` 才带服务端算好的 hunk；非流式请求返回同样的 JSON，测试走这条路径。

### 多轮：⌘K 是一段对话，不是一次生成

原来的协议每次生成都独立：用户说"再补一个超时分支"，模型收到的还是**原始选区**，于是它要么把上一轮已经做对的东西改回去，要么答非所问。对标 Cursor 的内联编辑，这一条是核心而不是细节。

做法是让请求带上 `history`（前几轮的指令 + 产出），服务端在**内存里**把上一轮的产出贴回原位置，构造一个"虚拟文件"，再把它交给模型：

```
磁盘：  func Add(a, b int) int { return a - b }      ← 文件没动
虚拟：  func Add(a, b int) int { return a + b }      ← 上一轮的产出
```

模型看到的是"文件已经带着我上一轮的改动"，于是新指令落在**上一轮的产出**上，而不是原始代码上。

关键在于：**三条不变量一个字都没改**。

- 响应里的 `old_text` 仍然是**磁盘上**的原文，不是虚拟文件里的内容；
- `base_hash` 仍然是磁盘文件的哈希；
- diff 仍然算在「磁盘原文 ↔ 最新产出」之间，所以用户看到的始终是**相对文件的累积改动**，而不是两轮之间的差异。

于是写回路径完全不用动：拿响应里的 `old_text` + `base_hash` 去 apply，写进去的就是最新一轮的产出，中间几轮被覆盖掉了——这正是内联编辑该有的样子。服务端另有一处 `aiEditVirtualRegion` 负责在内存里搭这个虚拟文件，复用写回路径的换行处理（插入模式要补后缀 `\n`，替换模式要继承原选区的换行），否则新代码会粘到相邻行上。

前端相应变成一个小对话：指令框在一轮成功后清空，按钮从「生成候选」变成「继续改」，标题显示当前是第几轮，并提供「从头再来」丢掉历史重来。

## 扩展点

### 加一个工具

1. 在 `pkg/tools` 实现，签名挂在 `Sandbox` 上（`Sandbox` 已有 `Web` / `Browser` 之类的可选能力字段）。
2. 在 `pkg/tools/call.go` 的 `callTool` 分发表里注册名字，并把常量加进 `knownToolNames`（`alias.go`）——否则"did you mean"永远指不到它。
3. 在 `NativeXxx()` 里补一份原生 `tool_calls` 声明（仅 `-native-tools` 用）。
4. 在 `Sandbox.DynamicToolDescriptions` 里加一行人类可读描述——**弱模型主要靠这段文字认识工具**。
5. 若工具只读，记得在 `previewArgs`（`agent` 包）里加一行，否则运行日志里看不到它在操作什么。

名字解析有两层，别搞混：`CanonicalToolName`（`pkg/tools/alias.go`）把模型凭记忆写出的别名换成真名，**在 agent 侧就做**（`canonicalizeToolNames`），因为 finish 拦截、编辑后自动校验、重复调用检测都按名字分支；`Sandbox.Call` 里也解析一次，是给绕过 agent 的调用方兜底。别名表只收语义唯一的名字：`search`（文件还是网页）和 `stop`（停运行还是结束回合）故意不收——猜错的别名比报错更坏。

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
