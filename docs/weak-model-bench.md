# 弱模型基准（divvy-bench）

引擎的立论是"用工程约束而不是模型聪明程度把复杂目标做完"。mock 端到端测试证明流程能跑通，但不能回答唯一重要的问题：**换一个真实的弱模型，成功率、成本、失败形态如何？** `cmd/bench` 就是回答这个问题的可重复 harness。

## 快速开始

```bash
go run ./cmd/bench -list     # 语料库一览

# 本机 Ollama（默认端点）：
go run ./cmd/bench \
  -base-url http://127.0.0.1:11434/v1 \
  -model qwen3.8:27b \
  -extra '{"reasoning_effort":"none"}' \
  -root bench-runs/$(date +%Y%m%d-%H%M%S)

# 任何 OpenAI 兼容端点：
go run ./cmd/bench -base-url https://api.example.com/v1 -model small-model -api-key ...
```

常用参数：`-goals string-utils,fix-average` 只跑部分目标；`-repeat N` 全库重复 N 轮（弱模型输出有方差，单轮数字别当真）；`-timeout` 单目标墙钟预算（默认 20m）；`-max-steps/-max-retries/-parallel/-isolate/-git-commit` 透传引擎旋钮；`-out` 指定 JSON 报告路径（默认 `<root>/report.json`）。

## 语料库

固定、版本化在 `cmd/bench/corpus.go` 里——基准的目标集一旦漂移，跨运行的比较就失去意义。五个目标覆盖五种任务形态（四个单文件 + 一个必须拆的多文件）：

| 目标 | 形态 | 验收命令（`Goal.Accept`） |
|---|---|---|
| `string-utils` | 从零创建包（3 个函数 + 表驱动测试） | `go build ./... && go test ./...` |
| `fix-average` | 修复 seeded 差一 bug（含空切片 NaN） | `go test ./...` |
| `http-health` | 构建可运行 HTTP 服务 + 单测 | `go build ./... && go test ./...` |
| `config-cli` | **多文件**：契约测试 + 3 个职责不同的文件（config.go / run.go / main.go） | `go build ./... && go test ./...` |
| `cmake-add` | CMake/C++ 目标 + ctest | `ctest --test-dir build --output-on-failure -R add` |

选 Go 是因为它是唯一能**完全离线自验收**的栈（`go build`/`go test` 不需要网络），基准因此是 hermetic 的：结果差异只来自模型和引擎，不来自外部服务抖动。

`fix-average` 的 workspace 自带一个**会失败**的测试——harness 测试（`TestBench_SeedWorkspaceIsolomorphic`）守着这条不变量：如果语料本身不失败，这个目标就退化成了"从零创建"，基准失去区分度。它同时考察引擎最核心的弱模型能力：读失败输出 → 定位 → 最小修复。

`config-cli` 是唯一**逼着拆**的目标：契约写在一个只有测试、没有实现的 workspace 里，提示词要求把实现分到 `config.go` / `run.go` / `main.go` 三个文件。它同时补上了另一个缺口——前面四个目标都是"单文件、可一次做完"，树模式的拆解成本在那里永远摊不开，`direct vs tree` 也就只能比较"最小修复"这一类。

这个目标的判定靠 `main_test.go`，所以它进了 `Goal.Guard`：跑完会比对受保护种子文件的字节，被改过或删掉就判失败（`protected seed file: main_test.go was modified`）。否则把断言删掉就能"通过"，基准测的就不再是模型实现契约的能力。`TestBench_GuardPathsAreSeeded` 守着"Guard 里的路径必须真的出现在 Seed 里"，免得一个笔误把反作弊检查悄悄关掉。

## 测哪个循环（`-loop tree|direct`）

```bash
go run ./cmd/bench -loop direct -goals fix-average -repeat 3
```

- **tree**（默认）：拆解 → 叶子 → 叶子 DoD 验收 → 根目标集成验收。成败由计划里的 DoD 决定，是引擎自己判的。
- **direct**：一段持久工具转录，没有计划、没有叶子、没有契约。`NewDirect(o, nil).Run(ctx)` 返回后，harness 自己跑 `Goal.Accept`——直接模式没有别的东西可以裁判它，靠模型自述完成度就是在测提示词而不是测能力。

同一条验收命令、同一套种子工作区，所以两行数字可以直接比较；差别只在"谁来跑验收"。报告里带 `loop` 字段，混跑的报告不会互相冒充。

宿主没有可用的执行后端（Windows，或 macOS 上 `sandbox-exec` 不可用）时 `run_bash` 直接不可用：直接模式的验收会标成 `skip` 并写明原因（`ACPT` 列），这一行**不算已验证**。测试 `TestBench_DirectLoopDrivesGoal` 特意做成不依赖 shell（脚本模型 + `write_file` + `finish`），这样 harness 本身在任何平台都能被验证，而验收只在能执行的地方发生。

## 采集的指标

每个目标一行：成败、墙钟、LLM 调用数、token、成本、叶子完成数，以及引擎自身的失败计数器（`verify_fail` / `retries` / `stalls` / `resplits`）。后者比成败更有信息量：一个"最终成功但重试 8 次"的模型和一个"一次过"的模型，在真实项目里的成本截然不同。

- token 取自任务树的全树累计（`TaskTree.TotalTokenUsage`），失败重试也计入——那正是基准要暴露的成本。
- 成本按内置价目表估算；本地模型（Ollama）不在表内，记为 $0，token 数仍然有效。
- 事件计数来自会话事件流（`agent.SummarizeEvents`），与 `-report` 复盘同一数据源。

## 工作区与事后解剖

每个目标一次运行一个目录：`<root>/<goal>-r<n>/`，内含 seed 的初始 git 提交、运行出的代码、以及该次运行的 `.divvy` 会话数据（树 JSON / 事件流 / 日志）。默认全部保留：

```bash
cd bench-runs/20261014-213000/fix-average-r1
git log --oneline            # 每个叶子一个提交（-git-commit 开启时）
git diff <seed>..HEAD        # 模型到底改了什么
divvy -report -session <id> -datadir .divvy   # 复用正式复盘
```

## 方法论注意事项

- **方差**：弱模型同题不同次的输出差异很大。结论请用 `-repeat 3` 以上的成功率区间，不要用单轮数字。
- **思考开关（本地大模型的头号预算杀手）**：`ollama show qwen3.8:27b` 显示该模型
  `capabilities.thinking` 的 **default 是 `medium`**，即默认就在思考。思考 token 不
  计入 `/v1/chat/completions` 的可见 `content`，但实打实消耗墙钟时间：实测一次
  **decompose 调用只产出 877 个响应 token，却耗时 981 秒（16.4 分钟）**——按
  6~7 tok/s 推算，约 5000 个 token 花在了不对外暴露的内部思考上，直接吃掉 20 分钟
  目标预算的大头，worker 只剩 3 分钟，最终 `context deadline exceeded`。
  同一提示词的 A/B（max_tokens=400）：

  | | 默认（thinking=medium） | `reasoning_effort=none` |
  |---|---|---|
  | 耗时 | 55.3s | **35.2s** |
  | completion tokens | 400 | **285** |
  | finish_reason | `length`（思考吃光预算，答案残缺） | **`stop`（完整作答）** |
  | 实际回答长度 | 55 字符 | **857 字符** |

  即：关掉思考后更快、更省，答案反而更完整。本地跑 27B 这类大模型时**两个阶段都
  建议关**：`-extra '{"reasoning_effort":"none"}'`。此前"拆解阶段保留思考质量更好"
  的说法来自 [qwen3.8-local.md](qwen3.8-local.md) 的小模型经验，在本地大模型的墙钟
  预算下不成立——若刻意要测"思考对成功率的帮助"，请把它当作一个待验证的旋钮，
  用 `-repeat` 多轮对比，而不是默认行为。
- **调参实验**：`-max-stall` 默认 3 对某些模型可能偏激进——这类"默认值是否合理"的问题正是基准要回答的：固定语料、只改一个旋钮、多轮重复。
- **harness 自身有测试**：`cmd/bench/bench_test.go` 用脚本化模型（`llm.ScriptedClient`）完整跑通一次目标运行，守住播种、git 基线、验收、指标采集全链路。harness 测试绿 = 基准失败可以归因于模型，不是工具坏了。

## 参考数字

来自 `docs/qwen3.8-local.md` 的一次手工运行（qwen3.8:27b @ Ollama，`reasoning_effort=none`，`-isolate -git-commit`）：「/health HTTP 服务 + 单测」任务，6 次 LLM 调用、5808 token、约 3.5 分钟完成，生成代码自带 200/405 两条测试。新跑法的报告 JSON 可与这组数字对照。

### 首轮真实基准（2026-10-15，qwen3.8:27b @ 本机 Ollama 0.34.4，M1 Pro/32GB）

**统一配置**（三目标同一把尺子：`reasoning_effort=none`，其余默认）：

```bash
go run ./cmd/bench -root bench-runs/<stamp> -extra '{"reasoning_effort":"none"}'
```

| 目标 | 结果 | 墙钟 | 调用 | token | 叶子 | VFAIL/RETRY/STALL |
|---|---|---|---|---|---|---|
| string-utils | ✅ | 15m17s | 32 | 79,094 | 6/6 | 0 / 0 / 0 |
| fix-average | ✅ | 1m33s | 4 | 6,600 | 1/1 | 0 / 0 / 0 |
| http-health | ✅ | 5m34s | 15 | 27,578 | 3/3 | 0 / 0 / 0 |
| **合计** | **3/3（100%）** | 22m24s | 51 | 113,272 | — | 全部为 0 |

三个产物均**跳出 divvy 独立复验**过，不是引擎自说自话：`go test ./...` 分别
`ok strutil`、`ok calc 0.982s`、`ok healthsrv 0.462s`；http-health 另实起服务验证
`GET /health → 200`、`POST /health → 405`。

**思考开关的同题对照**（同一语料库，只改这一个旋钮）：

| 目标 | 思考 medium（默认） | 思考 none |
|---|---|---|
| string-utils | ✅ 13m08s / 10 调用 / 27,428 tok / **1 叶** | ✅ 15m17s / 32 调用 / 79,094 tok / **6 叶** |
| fix-average | ✅ 2m15s / 5 调用 / 8,002 tok / 1 叶 | ✅ 1m33s / 4 调用 / 6,600 tok / 1 叶 |
| http-health | ❌ **20m 超时 / 0 叶完成** | ✅ **5m34s / 15 调用 / 27,578 tok / 3 叶** |

注意叶子数：开思考时模型倾向于把目标**判成一个原子叶子**（1 叶），拆解质量差且
墙钟被内部思考吞掉；关思考后正常拆分（6 叶 / 3 叶）。这也说明"拆解阶段应保留
思考"的直觉在本机大模型上不成立。

**首跑就咬出两个问题**，正好说明基准的价值：一是引擎缺陷（隔离模式下 DoD 里的
绝对路径让验收跑错目录，已修）；二是配置陷阱（思考默认 medium）。**结论：基准
失败时先分清是引擎接线、配置，还是模型能力**——这次的失败里没有一个真正属于
模型能力问题，全是我们自己的坑。

### 多轮重复：成功率与方差（`-repeat 3`，9 次运行）

```bash
go run ./cmd/bench -root bench-runs/<stamp> -repeat 3 -extra '{"reasoning_effort":"none"}'
```

| 目标 | r1 | r2 | r3 | 叶子数（三轮） |
|---|---|---|---|---|
| string-utils | 16m10s / 33 调用 / 83,116 tok | 13m25s / 32 / 71,913 | 17m51s / 31 / 74,757 | **6 / 6 / 6** |
| fix-average | 1m36s / 4 / 6,653 | 1m27s / 4 / 6,522 | 1m29s / 4 / 6,572 | **1 / 1 / 1** |
| http-health | 5m58s / 18 / 34,274 | 6m43s / 18 / 36,710 | 7m39s / 19 / 39,510 | **3 / 3 / 3** |

合计 **9/9 通过**，163 次调用、360,027 token、1h12m；
`verify-fail / retries / stalls / resplits` **全部为 0**。三轮产物同样经独立
`go test` 复验通过。

三点观察：

1. **拆解结构是稳定的**：同一目标三轮的叶子数完全一致（6/6/6、1/1/1、3/3/3），
   任务树形状不是随机漂移。
2. **难度决定稳定性**：fix-average（修 seeded 差一 bug）近乎确定性——4 次调用、
   6.5k token、1m30s，波动 <10%；string-utils 墙钟在 13m25s–17m51s 间浮动（约
   33%），差异主要来自生成长度，不是执行路径。
3. **引擎失败计数器全 0** 是最有说服力的一条：结合首跑对照，说明此前的失败全部
   归因于引擎缺陷 + 思考配置，**没有一个属于模型能力问题**。

关于统计口径要诚实：9 次全过不等于"成功率 100%"。按 rule of three，零失败的
95% 置信下界约为 `1 - 3/9 ≈ 66%`。引用时应写作"本样本 9/9，95% 下界约 66%"。

`-max-stall` 默认值（3）这份数据**无法判断**：9 次运行一次都没触发停滞检测。要
评价它是否偏激进，需要更弱的模型或更容易卡住的目标——这是一个待设计的实验，
而不是从现有数字能得出的结论。

### direct vs tree 对照（2026-11-14，M1 Pro/32GB，macOS `sandbox-exec`）

直接模式此前在 macOS 上根本没法验收（没有执行后端，报告里只能标 `skip`），所以这是 direct 的第一批真实数字。两边同目标、同种子工作区、同一条验收命令：

```bash
go run ./cmd/bench -loop direct -goals fix-average -extra '{"reasoning_effort":"none"}' -root bench-runs/<tag>
go run ./cmd/bench -loop tree   -goals fix-average -extra '{"reasoning_effort":"none"}' -root bench-runs/<tag>
```

| 运行 | 循环 | 成败 | 墙钟 | LLM 调用 | token | 验收 | VFAIL |
|---|---|---|---|---|---|---|---|
| 1 | direct | ✅ | 3m24s | 6 | 11,824 | pass | 0 |
| 2 | direct（下表两处修复后） | ✅ | 1m49s | 5 | 9,710 | pass | 0 |
| 1 | tree | ✅ | 2m31s | 4 | 7,672 | — | **1**（误计，见下） |
| 2 | tree（两处修复后） | ✅ | 1m47s | 4 | 7,715 | — | 0 |

读表前先读这三条：

1. **n=1，同一模型同一目标，方差比差异大。** 第 2 次 direct 只用了 5 次调用，不是因为改了什么，而是这次模型没再发明工具名（第 1 次它开局调用了不存在的 `batch_read`）。墙钟的改善还有一部分来自模型已在显存、prompt 缓存命中——第一次含 20~40 秒加载。**比较以 calls / tokens 为主，墙钟只当参考。**
2. **`fix-average` 是树模式最有利的形态。** 拆解判成 atomic（1.8k token）后退化成 1 个叶子，tree = decompose + 3 步 worker = 4 次调用；direct 的开销在别处：每步系统提示都带项目地图、声明索引和项目规则，5 次调用约 9.7k prompt token。要看清两条路的差别，语料需要一个**必须拆**的多文件目标——这是语料库的下一步。
3. **两边都一次做对**，没有重试、停滞或重新拆解。所以这张表还测不出"模型第一次改错"时的恢复成本，而那正是引擎立论所在；要看那条得换更难的语料或更小的模型。

和 2026-10-15 那轮对照：同一目标的 tree 是 4 次调用 / 6,600 token，这次 4 次 / 7,715 token（+17%），**调用数完全一致**，说明树模式在这个目标上的执行路径是稳定的；direct 的 5~6 次则取决于模型开局怎么读文件。

#### 这一轮跑出来、并且已经修掉的两件事

- **`batch_read`**：模型凭记忆编工具名，代价是一整次 LLM 往返（本机约 60 秒、1.9k token）。现在有一张不歧义别名表接住它（`pkg/tools/alias.go`，见 README「工具名写错不会浪费一轮」）。
- **VFAIL 把基线检查算成了失败**：跑之前引擎会先探一眼没动过的工作区（区分"本来就坏"和"这一波弄坏的"），`fix-average` 的种子 bug 让这一步天然失败，于是零失败的运行也显示 `VFAIL=1`。会撒谎的指标比没有指标更糟，所以基线改记 `wave_baseline` 事件、不进 VFAIL。表里第 1 次 tree 的 1 就是这个误计。

### `config-cli`：第一个必须拆的目标（2026-11-14）

| 运行 | 循环 | 叶子步数 | 成败 | 墙钟 | 调用 | token | 叶子 | 说明 |
|---|---|---|---|---|---|---|---|---|
| 1 | direct | 40 | ❌ | 6m31s | 19 | 60,689 | — | 3 次连续非法 JSON 中止（根因见下） |
| 2 | direct | 40 | ✅ | 2m06s | 9 | 21,384 | — | 修掉 VCS stamping 之后 |
| 3 | tree | 15 | ✅ | 3m10s | 7 | 16,808 | 1/1 | 拆解仍判 atomic，没拆 |
| 4 | tree | 6 | ❌ | 14m36s | 23 | 58,670 | 4/4 | 拆成 4 叶全完成，败在根验收 |
| 5 | tree | 6 | ✅ | 20m51s | 18 | 51,257 | 1/1 | 计划结构修复后重跑；根验收通过（墙钟被机器负载抬高，见下） |

第 1 次的 19 次调用 / 60k token **不是模型能力问题**：`go build` 会给 main 包做 VCS stamping，而沙箱里 git 跑不起来（macOS 上 `xcode-select` 被拒），于是每一条 `[auto-verify] go build .` 都报 `error obtaining VCS status`。模型把它当代码错误，先后去查 git、再想 `xcode-select`、再试 `sudo`（被沙箱挡住），十步之后开始输出截断的 JSON。修法是把 `GOFLAGS=-buildvcs=false` 强制进沙箱环境（`pkg/tools/sandbox_isolate_env.go`，两个平台共用）——沙箱里本来就没有有意义的 VCS 状态可打。回归测试 `TestDarwinBackend_BuildsMainPackageInGitRepo` 把这条钉住：去掉修复，它立刻复现同一条 `error obtaining VCS status`。修完后同一目标 9 次调用、2 分 06 秒通过。

第 3 次说明**拆解器的原子偏置比任务规模更强**：即使任务明确要求三个文件，15 步预算下仍判成 atomic。

第 4 次把叶子预算压到 6（`Goal.LeafSteps`，见 `config-cli` 定义），真的拆成 4 叶、全部 COMPLETED，但根验收失败。失败的是模型自己写的 DoD：

```
go test ./... && go build -o /tmp/config_cli . && /tmp/config_cli test.json
```

三件事都发生过，而引擎当时一件也没拦住：

1. **macOS 沙箱没有可写的 `/tmp`**（只有被重定向的 `TMPDIR`），`go build -o /tmp/config_cli` 直接 `mkdir /tmp/: operation not permitted`。bwrap 后端能给私有 tmpfs，seatbelt 不能，只能拒绝——这是安全策略差异，不是 bug，但模型生成的 DoD 里 `/tmp` 是最常见的临时路径。
2. **`test.json` 谁都不负责产出**：仓库里没有，计划里也没有叶子声明它。引擎到目前为止只检查"DoD 引用的路径有没有人产出"（`planBlockers` 的 outputs 侧），不检查**输入**。
3. **重试路径原本等于空转。** 验收失败被路由到集成叶子 `impl_main`，但它的重跑是**盲的**：`handleAcceptanceFailure` 把错误写进 `node.ErrorMsg`，而 `executeLeaf` 的 `prevErr` 永远从空字符串开始，两边各说各话。于是叶子满足自己那个更窄的 DoD、合并、根验收以完全相同的方式失败，2 次重试用尽判 FAILED——14 分钟里没有任何一次重试是有信息的。

第 3 条已在本轮修掉（`938aae7`）：重新排队时给 `ErrorMsg` 打 `[goal-acceptance]` 前缀并带上失败命令与输出，`executeLeaf` 把它取作 `prevErr` 初值、取完即清；文案明写"你自己的 DoD 过了不算数"。回归测试 `TestExecuteLeaf_SeedsRetryPromptFromAcceptanceFailure` 把叶子 DoD 设成 `true` 来隔离变量——它自己必然通过，那么 prompt 里出现的任何失败只可能来自根验收。

**上表的数字全部产生于这次修复之前**，所以第 4 行的那次失败不能用来判断修复后的表现；`config-cli` 的 tree 侧目前仍是 known-failing，等空闲时重跑再登记（本机 load 10+ 时 27B 模型的吞吐会掉到几分之一，单次 decompose 就能挂住十几分钟，那种数据没有意义）。

### `config-cli` tree：修复后第一次重跑，第 5 行 ✅（2026-11-14 晚）

计划结构修复（`pkg/agent/acceptcmd.go`，PR #22）之后重跑 tree 侧：**通过**。

```
config-cli  tree  1  yes  20m51.185s  18  51257  1/1  -  3  1  0  0
                                       ↑根验收过  ↑墙钟  ↑调用 ↑token ↑VFAIL ↑重试 ↑停滞
```

模型这次给的根验收命令是一条**链**：

```
go build ./... && go test ./... && go build -o /tmp/configcli . && echo '{"name":"svc","retries":3}' > /tmp/cfg.json && /tmp/configcli /tmp/cfg.json
```

这条命令同时踩中了第 4 次失败的三个坑，又恰好全躲过去了：

- `/tmp` 的写入这次用的是 `> /tmp/cfg.json` **重定向**，走的是沙箱允许的路径；`-o /tmp/configcli` 的产物路径不再被当成"某个叶子必须交付的文件"（工作区外的路径没有人能拥有它）；
- 夹具由命令自己 `echo` 出来，**不再需要某个叶子去产出 `test.json`**——上一轮"谁都不负责产出输入"的坑这次模型自己绕开了；
- `VFAIL=3` + `RETRY=1` 说明叶子真的撞了三次失败的验证并自修回来，不是第一次就绿。

`resplits=0` 是这次修复的可见痕迹：按旧的解析，这条命令里的 `&&` 会被当成一个必须交付的路径，`/tmp/configcli` 也会被当成交付物 → 两个**谁也修不掉的假 blocker** → 白烧 `-max-redecompose` 预算并逼规划器去修不存在的问题。现在这两个都不再成立。

**但这一行的墙钟和 token 不能跟前四行比。** 本轮机器负载 9+（用户自己的编译在跑），第一次 decompose 单步就花了 12 分钟；`20m51s` 里绝大部分是排队等模型，不是引擎慢。`load` 回落到 2 以下时必须再跑一次才能拿到可比的数字。

**这次也没有真正压到拆解路径**：模型又判了 atomic（1/1 叶），所以 `planBlockers` 在这次运行里没有被调用。假 blocker 的证据在单元测试里（把 `splitCommandChain` 改回"整条命令一袋词"，8 个用例立刻报红，报红形态与线上 bug 一致），不在这次基准里。

前两条都还是**未决**，而且都不是"改个参数"能解决的：

- **沙箱要不要给一个可写的 `/tmp`。** 现状是**不给，但把话说清楚**：`[sandbox]` 提示 + 提示词里预先写明"临时文件放工作区或 `$TMPDIR`"（见 `sandbox_hint.go` 那节）。理由是放开 `/tmp` 等于承认 macOS 上没有私有临时目录（可写就意味着沙箱进程能读别的进程放进去的文件）；而"看不懂的错误被误当成 bug"这个问题，不用动策略也能解。剩下的是观察：模型会不会照着提示改。如果实测里它仍然反复撞 `/tmp`，那才需要重新讨论这条边界。
- **集成叶子的 DoD 要不要并入根验收命令。** 现在只做到"把失败命令告诉它"（种子），叶子自己的验收门仍然比根的门窄，等于"你告诉我门没锁，我去擦了窗户"的一半。改成继承才是结构性的；犹豫的地方是继承会让叶子的验收依赖一条它没有能力满足的命令（例如需要整棵树、或用户环境的命令），那时叶子会卡在自己的 verify 上，比现在更难诊断。
