# divvy

*An automated coding agent for small, cheap LLMs: split big goals into contract-driven task trees, execute each leaf in an isolated context, and verify with real builds and tests.*

面向弱模型（小参数、低算力、廉价模型）的自动化编程 Agent。  
核心策略：**愚公移山，分而治之** —— 把大目标拆成带契约和验收命令的任务树，每个叶子在隔离上下文里执行，靠编译/测试而不是长会话记忆交付结果。

---

## 快速开始

```bash
go install github.com/esrrhs/divvy/cmd/divvy@latest
# 或在仓库内：
go build -o divvy ./cmd/divvy
```

任意 **OpenAI 兼容** 接口都可以，包括 OpenAI、vLLM、Ollama、本地网关。本地 Ollama 实战配置见 [docs/qwen3.8-local.md](docs/qwen3.8-local.md)、[docs/debug-local-ollama.md](docs/debug-local-ollama.md)；想改引擎内部看 [docs/architecture.md](docs/architecture.md)：

```bash
export OPENAI_API_KEY=sk-...
export OPENAI_BASE_URL=http://127.0.0.1:11434/v1   # 本地模型带 /v1
export OPENAI_MODEL=qwen2.5-coder:14b

./divvy -workdir ./ws "用 Go 写一个 /health 返回 ok 的 HTTP 服务，并带单测"
```

先只拆解不执行，人工检查任务树后再跑：

```bash
./divvy -plan -workdir ./ws "目标"   # 生成任务树后退出
./divvy -resume -workdir ./ws        # 执行已规划的任务
```

常用参数：

| 参数 | 含义 |
|---|---|
| `-workdir` | 代码落地目录（工具只能读写这里） |
| `-resume` | 从上次会话继续（默认读 `.divvy/LATEST`） |
| `-session` | 指定会话 ID |
| `-status` | 只打印任务树，不执行 |
| `-plan` | 只拆解出任务树并保存，不执行（配合 `-resume` 使用） |
| `-strict` | 配合 `-plan`：有契约告警时以非零码退出（CI 自动把关） |
| `-git-commit` | 每个叶子合并后自动 `git commit`（workdir 需为 git 仓库），哈希记入叶子摘要 |
| `-parallel` | 同时执行的叶子数，默认 `1`；>1 时自动开启 `-isolate` |
| `-isolate` | 叶子在主工作区的临时镜像里执行，验收通过才合并回主工作区，失败即丢弃 |
| `-max-retries` | 叶子验收失败最多重试几次，`0`（默认）为无限 |
| `-max-stall` | 同一个失败**连续**出现 N 次后停止重试，转去重新拆解或判失败，`0` 为不限（默认 `3`） |
| `-max-elapsed` | 单个叶子的墙钟预算（跨其所有尝试累计），默认 `45m`，`0` 为不限；`-max-retries 0` 时的安全网 |
| `-retry-max-wait` | 指数退避上限，默认 `30s` |
| `-native-tools` | 改用 OpenAI `tool_calls`（强模型可开；弱模型默认 JSON 更稳） |
| `-extra` | 合并进请求体的 JSON，例如 Qwen3：`'{"enable_thinking":false}'` |
| `-max-cost` | 会话成本上限（美元），含 resume 之前的花费；超限即停并保存，`0` 为不限 |
| `-budget-tokens` | 会话 token 上限，含 resume 之前的花费；超限即停并保存，`0` 为不限 |
| `-pricing` | 自定义价目表：JSON 文本或 JSON 文件路径，如 `'{"my-model":{"input":0.15,"output":0.6}}'`（每百万 token 美元价） |
| `-sessions` | 列出已保存的会话（状态、叶子进度、目标），不执行 |
| `-report` | 打印某个会话的复盘汇总（目标/进度/成本/重试与停滞/失败原因），不执行 |
| `-prune` | 删除旧会话，只保留最近的 `-keep` 个（含其日志与事件流；`LATEST` 指向的会话永不删） |
| `-keep` | 配合 `-prune` 保留几个最近会话，默认 `5` |
| `-dry-run` | 配合 `-prune`：只列出将被删除的内容，不真删 |
| `-v` | 打印模型原文和工具输出（结束时附带分项 token 用量） |

中断（Ctrl+C）会保存任务树，之后：

```bash
./divvy -resume -workdir ./ws
```

---

## 运行时在做什么

```
根目标
  └─ Decomposer 输出 JSON（原子？或 2~6 个子任务 + 契约 + 验收命令）
        └─ 叶子 Worker：干净上下文 + 9 个基础工具
              list_dir / read_file / write_file / replace_lines / delete_path / move_path / run_bash / search_files / find_files
              └─ Verifier 跑 DoD 命令（如 go test ./...）
                    ├─ 通过 → 向上冒泡 COMPLETED
                    └─ 失败 → 新的隔离上下文重试（带上错误，指数退避，上限 30s，默认无限次）
```

叶子执行**不携带**其它叶子的对话历史，只注入：当前任务、契约、父节点/依赖摘要、少量相关文件、验收命令。
`search_files` 用正则搜索文件内容并返回紧凑的 `相对路径:行号:匹配行`：修改现有代码时，弱模型用它一次定位符号，不必逐个读整个文件，省步骤也省上下文（自动跳过 `.git` 等目录，可用 `glob` 过滤，默认忽略大小写）。`find_files` 则按文件名 glob 查找（`*_test.go` 裸模式按 basename 递归匹配任意深度，`pkg/*.go` 按相对路径匹配），返回路径列表。
`read_file` 支持可选的 `start_line`/`end_line`（1-indexed、含端点，返回带行号的片段）：配合 `search_files` 的行号只读目标区段，大文件也能直接跳到 64KB 整读截断点之后，无需在 shell 里拼 `sed`。也支持 `{"paths":["a.go","b.go"]}` **一次批量读最多 8 个文件**（各自独立套 64KB 上限，带 `### path` 分隔头）——弱模型每轮只能调一个工具，批量读把"看 N 个相关文件"从 N 次 LLM 往返压成 1 次；任一路径不存在则整批失败，不会返回半截结果。`run_bash` 支持 `timeout_sec`（默认 60s），跑 `npm install`、`cargo build` 这类慢命令时显式放大超时。
`replace_lines` 除行号区间和单点 `old_string`/`new_string` 外，还接受 `edits: [{old_string,new_string}…]` **批量原子编辑**：所有锚点先在内存里逐条校验（缺失或不唯一即整体失败、文件一字节都不改），全部通过后才一次写回——同一文件改多处不必串行多轮，也不会留下半改状态，用文本锚点还能避开行号漂移。`delete_path`（删目录必须显式 `recursive:true`，工作区根目录受保护）和 `move_path`（工作区内重命名/移动，禁止移入自身子树）让删除和重命名走沙箱校验，不用再借 `run_bash` 拼 `rm`/`mv`。
验收失败时，Verifier 会按已探测的技术栈从输出里抽取去重、单行截断的根因诊断（最多 25 行）作为**诊断块前置**到错误全文之前；重试叶子只截取错误前缀注入上下文（4000 字符），根因因此一定在最显眼的位置，原始日志完整附在块后。目前覆盖：Go（`x.go:4:2: undefined:`、`--- FAIL`、`panic`）、Python（traceback 的 `File "x", line N`、`XError:`、pytest 的 `E` 行与 `FAILED`）、Rust（`error[E0xxx]:`、`--> file:line:col`、`panicked at`）、Node/TypeScript（tsc 的 `x.ts(10,5): error TS…`、Node 的源码位置行与 `ReferenceError:` 等异常头；刻意不收冗长的 `at …` 栈帧）。
重试不是无条件的。每次失败都会算一个**指纹**（优先用上面那套根因诊断行，没有诊断规则时取输出头部并把数字统一掩码），连续 `-max-stall` 次（默认 `3`）指纹相同，就判定这个叶子在**原地打转**——它失败得很快，但每次失败都一样，时间闸门永远等不到。此时停止重试，转去重新拆解（拆解预算还有的话）或直接判 `FAILED`，并记一条 `leaf_stall` 事件。失败**各不相同**的叶子不受影响，照旧重试到 `-max-retries` / `-max-elapsed`。这是默认 `-max-retries 0` 下无限重试能真正收敛的关键：在此之前，只有 45 分钟的墙钟能结束一个卡死的叶子。

叶子用完 `-max-steps`（默认 20）还没调用 `finish` **一律算失败**，不会送去验收：模型没有声称完成，文件可能是半截的，finish 前的 `review_diff` 自检也还没跑。（早先这里会直接进验收，碰上 `ls` 这类占位 DoD，一个没干完的叶子会被判成 COMPLETED。）最后 3 次工具调用时 worker 会附一条 `[budget] N tool call(s) left` 提醒——只进当次请求、不写进对话历史，避免后续步骤读到过期的计数。

没有依赖关系的就绪叶子可以并发执行（`-parallel N`，默认 `1`）；每次 LLM 调用的 token 用量按节点记入任务树并随会话持久化，运行结束打印本次与会话累计（resume 后自动累加），树状进度与 `-status` 里也会显示每个节点的消耗。

开启隔离（`-isolate`，`-parallel >1` 时自动生效）后，叶子在主工作区的**临时镜像副本**里写代码、跑验收命令：
验收通过才把新增/修改的文件合并回主工作区（删除也会传播），失败或中断则整个镜像丢弃——失败的尝试永远不会污染共享工作区或兄弟叶子。

并发写同一个文件会互相覆盖，因此有两道防线：

- **调度层（预防）**：声明了相同产出文件的叶子被互斥调度（`-parallel N` 也不会同时跑），路径按 `./a.go`、`a//go` 等价归一。
- **合并层（兜底）**：契约没写产出时调度层无从判断，此时镜像合并会比对快照时的内容摘要——发现该文件在本叶子改动之后又被别人改过，就**整批放弃合并**（不覆盖、不半合并），该叶子重建镜像重试，新镜像已含兄弟叶子的成果，失败因此变成一次"对齐"。

### 集成叶子与根目标验收

每个叶子的 DoD 是局部的（某包 `go build`/`go test`），全部叶子通过**不等于**用户目标达成——实测中曾出现：所有包编译、测试通过，却漏掉 `main.go`，整个东西根本不能运行。为此：

- **集成叶子**：当目标是可运行的程序/服务/API/CLI 时，拆解强制包含最后一个 `integrate` 叶子，负责把各层组装起来、创建入口（`main.go`），并依赖所有其他叶子。
- **根目标验收**：复合根在所有叶子完成后，再跑一次根级 DoD——必须**真正运行交付物并端到端验证**（启动服务、用 `curl -f` 打真实接口、再停掉；CLI 则用真实参数执行），而不只是编译某个包。验收通过（`IntegrationVerified`）根才允许 `COMPLETED`。
- 验收失败会把错误**回路由给集成叶子**，它带着错误重新执行并修复装配，指数退避、受 `-max-retries` 约束；若计划里根本没有集成叶子（无法自愈），根直接 `FAILED`，明确提示而不是假装完成。
- `-plan`/`-strict` 会对这类缺口告警：可运行目标却无入口叶子、无根级验收命令。

### 多语言项目验收

默认 DoD 命令不再只认 Go。引擎会按工作区里的标记文件**自动识别技术栈**（多标记共存时固定优先级），并为叶子和根目标生成对应的验收命令：

| 技术栈 | 识别标记 | 默认叶子验收 | 默认根验收 |
|---|---|---|---|
| Go | `go.mod` | `go test ./...` | `go build .` + `go build ./...` |
| Node.js | `package.json` | `npm test --if-present` | `npm test --if-present` |
| Rust | `Cargo.toml` | `cargo test` | `cargo build` |
| Python | `pyproject.toml` / `requirements.txt` / `setup.py` | `python3 -m py_compile` 全量语法检查 | 同左 |
| Makefile | `Makefile` | 产出文件存在性检查 | `ls` |
| 通用 | 无标记 | 产出文件存在性检查（无产出则 `ls`） | `ls` |

要点：

- 模型在 DoD 里**显式给出的命令永远优先**，默认值只在它没写命令时兜底；拆解提示中会注入"Detected toolchain: X"，弱模型据此直接产出对应栈的命令。
- Python 默认用 `py_compile` 做语法检查（`compileall` 即使遇到语法错误也返回退出码 0，不能用于验收）；无 `.py` 文件时该命令自动跳过。
- Node 用 `--if-present`：包没定义 test 脚本时退出 0，而不是 npm 的 "no test specified" 误报。
- 没有任何标记的全新空目录仍是"通用"，建议先放入脚手架（`package.json` 等）或直接用 `-plan` 检查默认 DoD 是否合理。

### 联网搜索与抓取

默认完全离线；加上 `-web` 后叶子获得三个网络工具，用于查询模型自身无法获知的最新文档/版本，以及下载交付所需的文件：

- `web_search`：`{"query":"...","max_results":5}` → 编号结果（标题/URL/摘要）。
- `web_fetch`：`{"url":"https://..."}` → 抓取单个页面，**HTML 自动转成纯文本**（剥离 script/style），文本/JSON/XML 原样返回。
- `download_file`：`{"url":"https://...","path":"assets/x.bin"}` → 把 URL 内容**按字节**存入工作区文件（web_fetch 只回文本，下载图片/压缩包/二进制用它），返回字节数与 content-type；200 以外状态码报错，**失败时自动删除半成品文件**，20MB 硬上限（超限拒绝而非静默截断）。

```bash
./divvy -web -workdir ./ws "查一下 X 的最新 API，写一个调用示例"
```

搜索后端可通过 `-search-url` 配置（模板必须含 `{query}`）：

- 留空：内置 **DuckDuckGo lite**，无需 API Key（仅解析公开 HTML，可能被网络策略拦截）。
- **SearXNG**：`-search-url 'http://host/search?q={query}&format=json'`，走 JSON，适合自建稳定检索。

安全约束（web_fetch、download_file 与每一跳重定向都会执行）：只允许 http/https；目标域名解析后若指向回环/私网/链路本地/CGNAT 等非公开地址一律拒绝；只允许 80/443；重定向到内网同样拦截。因此 agent 无法借抓取或下载访问本机或内网服务。

### 浏览器、HTTP、Git、代码理解工具

除网络搜索外，工具集还包含以下能力（按开关/环境可用）：

**Headless 浏览器（`-browser`，需 Chrome/Chromium）**

- `browser_navigate`：加载 **JS 渲染页面**，返回标题 + 渲染后文本（补 web_fetch 拿不到 SPA 内容的缺口）；
- `browser_click`（CSS 选择器）、`browser_type`（输入文本，可先清空）、`browser_text`（读取元素/正文渲染文本）；
- `browser_screenshot`：整页 **PNG** 存入工作区（`{"path":"shot.png"}`）。
- 浏览器进程懒启动、跨叶子复用同一标签页；用户中断时才关闭。这也让 Web 前端交付物具备"真实浏览器交互"级别的验收手段。
- `-parallel >1` 时多个叶子共享同一个浏览器：**叶子级租约**保证一个叶子的 navigate→click→type 多步流程不被另一个叶子的导航插队（per-action 锁只防数据竞争，防不了跨叶子串台）。叶子在首次浏览器工具调用时取租约、worker 结束（含失败/中断）时释放；等待取约可被 ctx 取消，从不用浏览器的叶子完全不受串行化影响。

**结构化 HTTP（`-web` 下）**

- `http_request`：`{"url","method","headers","body"}`，返回状态码/响应头/正文；弱模型不必在 shell 里手拼 curl。

**Git 只读工具（工作区为 git 仓库时）**

- `git_status`、`git_diff`（可选 `staged`/`path`）、`git_log`（可选 `limit`/`path`）；均为只读，不改动仓库。
- `review_diff`（可选 `staged`）：finish 前的**确定性自检**，解析 `git diff --unified=0` 只检查新增行——未解决的冲突标记（`<<<<<<<`/`=======`/`>>>>>>>`）、误留的调试语句（`fmt.Print*`、`println`、`console.log`、`debugger`、`pdb.set_trace`、`breakpoint`）、高置信硬编码密钥（私钥头、`AKIA…`、Slack token、带引号字面量的 `token/password/secret/api_key`），以及超过 600 行的大 diff 告警；每类最多列 10 条并提示省略数。干净时返回 `review_diff: clean (...)`。
- **finish 自动门控**：叶子调用 finish 时引擎自动跑一次覆盖**未跟踪新文件**的 review（`git diff` 不包含新建文件，门控额外扫描 `git ls-files --others` 的文本文件，二进制自动跳过）。发现冲突标记或硬编码密钥则**拒绝 finish**，把问题清单作为工具结果回灌，叶子在新的一轮里修复后才能完成；调试残留和大 diff 只记录告警、不拦截。门控出错（非仓库/git 异常）时 fail-open，不会因为检查工具本身故障卡死交付。

**代码/数据理解（默认可用）**

- `find_symbol`：按名字定位定义——**Go 走真实 AST**（func/type/var 精确到行），其它语言回退正则；加 `references:true` 则做**文本级引用扫描**（词边界匹配、过滤函数/类型/变量声明行，复用 `search_files` 的输出形态；不做类型分析，注释和字符串里的同名提及也会出现，别名/动态调用会漏掉），改签名前先看谁在用；
- `outline`：`{"path":"x.go"}` 返回单个文件的**顶层声明地图**（带行号）：Go 走真实 AST，func/方法显示完整签名、type 显示底层形状（`type Widget struct`）、函数体一律省略；其它语言按 def/class/fn/struct/enum/trait 等声明模式回退。探索陌生文件时先 outline 拿地图，再用 `read_file` 的行号区间精读，比整读省上下文、比 find_symbol（需先知名字）更适合浏览；
- `json_query`：用点号/方括号路径（`items.0.name`、`items[0].name`）从大 JSON 里抽单个值，避免整文件进上下文。
- `sqlite_query`：`{"path":"data.db","query":"SELECT ...","limit":50}` 对工作区内的 SQLite 数据库跑**只读**查询（`mode=ro` + `query_only`，只接受单条 SELECT/WITH/PRAGMA/EXPLAIN，带分号的多语句直接拒绝）；返回 `columns:` 头加每行一个 JSON 数组，NULL 正常显示、BLOB 标注字节数、单元格超 500 字符截断，默认 50 行（上限 200）。使用**纯 Go 驱动** [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)，无需 cgo 或本机 sqlite3 CLI——弱模型不必再拼命令行、也不会因环境缺工具而失败。

### 交互式 REPL（工头 + 独立叶子）

`-interactive` 进入多轮交互，但它**不是**一条长会话：结构上是一个轻量"工头"对话层 + 独立上下文的叶子工人，与批处理坚持同一哲学。

```
你的需求
  └─ 工头（foreman）：理解意图、决定派发，自身没有文件/shell 工具
       └─ dispatch：把工作拆成自包含任务，每个叶子独立上下文执行
             （叶子看不到对话，只拿到：任务说明 + 技术栈 + 工作区快照 + 压缩工作日志）
             → 叶子只回报简短摘要
       └─ 工头据摘要继续派发，或 respond 回复你
  本轮结束 → 压缩成一条结构化摘要（不是原文），只保留最近 8 轮
```

- **叶子独立上下文**：实际编码全部发生在叶子里（复用 worker），工头只看摘要；每片叶子互不携带对话历史，和批处理一致。
- **多轮连贯靠压缩工作日志**：可以继续说"把那个改成…"，工头会把指代解析成具体路径写进叶子说明；工作日志只保留最近 8 轮的压缩摘要（用户请求/叶子结果/回复），不保留原文，弱模型也不会被长历史拖垮。
- 每个叶子派发时显示 `-> leaf N: <title>`；需求不明确时工头直接用回复提问，而不是盲目开工。
- 斜杠命令：`/help`、`/clear`（清空工作日志、不动文件）、`/status`（调用次数、foreman/叶子分类 token、**总会话 token 与估算美元成本**、日志条数；无价模型会提示用 `-pricing` 配置）、`/exit`（或 Ctrl-D）。

```bash
go build -o divvy ./cmd/divvy
./divvy -interactive \
  -base-url http://127.0.0.1:11434/v1 -model qwen3.8:27b -workdir .
```

位置参数会作为第一条需求自动执行；交互会话默认不持久化任务树（`-datadir` 使用临时目录），命令结束时其后台进程会被清理，长驻服务请在单独终端运行。

### 引导式工作流（人在回路）

`-guided` 在批处理自动交付的基础上加入人在回路：**先规划、给你过目、敲定后再执行**，执行中还能改计划、回答 agent 的提问。

```
规划（拆成树） → 评审 ← 你提调整意见则重新规划
                     ← /approve 敲定、/abort 放弃
                → 执行（每步显示进度）
                     ├─ agent 用 ask 向你提问，你回答后继续
                     └─ 你可 /add 加叶子、/redo <id> 重做、/plan 看全貌、/pause 暂停保存
                → 根目标端到端验收 → 完成
```

评审阶段直接输入调整意见（如"把存储和模型拆成两个"、"再加一个命令行入口"），引擎会据此重新规划并再次展示，直到你输入 `/approve`（也支持中文"开始/执行/确认"）。执行中：

- `/add <说明>`：把新需求作为叶子加入，根回到未完成、随后执行它；也可在"无叶子可跑"时直接输入说明。
- `/redo <id>`：把指定节点重置为待执行。
- `/pause`：安全停下并保存任务树，用 `-guided -resume -session <id>` 继续（已开工的会话恢复时跳过评审直接执行）。
- `/plan`：随时重新打印当前任务树，查看还剩什么，不影响执行。

```bash
./divvy -guided -isolate -git-commit \
  -base-url http://127.0.0.1:11434/v1 -model qwen3.8:27b -workdir ./ws "你的目标"
```

`-plan` 在拆解完成后会做完整性检查并对弱契约告警：无验收命令、验收只有占位符（`ls`）、无产出声明、兄弟叶子声明了相同产出文件。相同的检查在正常执行时也会跑（告警逐条打印并记入事件流），不必特意先跑一次 `-plan` 才发现契约很弱。

### Web 界面（`-serve`）

`-serve` 在本机启动一个只绑定 `127.0.0.1` 的 HTTP + SSE 服务，并自动打开浏览器；终端会打印带一次性随机 token 的访问 URL（每次启动重新生成，浏览器之外的页面无法调用 API，Host 头仅允许 localhost/127.0.0.1）。

```bash
./divvy -serve -workdir ./ws                 # 随机端口、自动开浏览器
./divvy -serve -port 8787 -no-open           # 固定端口、不自动打开
```

在界面里可以：新建会话（填 goal/model/baseURL/apiKey，apiKey 只在内存中，不写入事件流与日志）、实时看任务树与事件流、审批/调整/中止计划、逐叶子审查 diff（`approval_mode=manual`，批准才合并、拒绝带意见重做）、回答 ask、暂停/续跑/add/redo、浏览历史会话与只读查看工作区文件。Ctrl+C 会优雅关闭：运行中的会话先保存任务树，之后可在界面或用 `-guided -resume -session <id>` 继续。

### 桌面应用（Tauri，macOS）

`desktop/` 是 Tauri v2 外壳：启动时按平台定位并拉起内置的 `divvy -serve` sidecar（externalBin），解析其启动日志里的随机端口与一次性 token，再把 WebView 指向 sidecar（发布模式）或 Vite（开发模式，`/api` 代理到 sidecar）。数据与默认工作区在 `~/Library/Application Support/com.divvy.desktop/`；关闭窗口或退出应用时先给 sidecar 发 SIGINT 让运行中的会话 checkpoint，等待 12s 兜底后再强杀，不留僵尸进程；sidecar 启动或 URL/token 解析失败会弹原生错误框并以非零码退出。

macOS 前置（首次）：

```bash
# 1. Xcode 命令行工具（clang/WebKit 由它提供）
xcode-select --install
# 2. Rust 工具链
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
# 3. Go（构建 sidecar 用）与 Node.js（构建前端用）
```

开发模式（热更新前端；首次会先 `go build` sidecar 再拉起 Vite）：

```bash
cd desktop
npm ci
npm run dev          # = tauri dev；脚本会自动构建 binaries/divvy-sidecar-<triple>
```

发布构建（构建前端 → 构建 sidecar → 打包 `.app`/`.dmg`）：

```bash
cd desktop && npm run build
# 产物：target/release/bundle/macos/divvy.app、target/release/bundle/dmg/*.dmg
```

sidecar 二进制按 `$GOOS/$GOARCH` 映射命名（`binaries/divvy-sidecar-aarch64-apple-darwin` 等），可单独运行 `desktop/scripts/build-sidecar.sh` 刷新。排障可用环境变量覆盖：`DIVVY_SIDECAR`（替换后端程序路径）、`DIVVY_SIDECAR_PORT`、`DIVVY_SIDECAR_STARTUP_TIMEOUT_MS`。

### 成本估算与预算护栏

引擎内置常见 OpenAI 模型的近似价目（每百万 token 美元价），用 `-pricing` 可覆盖或追加自有/本地模型的价格（JSON 文本或文件路径，键名同时支持精确匹配和最长子串匹配）。

- 每次调用的成本按节点随 token 用量一起显示：树视图节点标签、结束/`-status` 用量汇总（本次 + 会话累计）。
- `-max-cost`（美元）与 `-budget-tokens` 是**会话级硬上限**，计入 resume 之前已持久化的花费；超限立即取消运行、把在途节点复位为 `PENDING` 并保存任务树。提高上限后用 `-resume` 继续即可，不会重试或重复烧钱。
- 模型在价目表中无对应价格时不显示估算（本地零成本模型的典型情况），token 预算仍然生效。

四层闸门互相独立：会话级 `-max-cost` / `-budget-tokens` 管总量；单叶子 `-max-elapsed`（默认 `45m`）管"一个慢叶子"；`-max-stall`（默认 `3`）管"一个快但在原地打转的叶子"；`-max-steps` 管单次尝试的收尾。

> `-max-retries 0`（默认）表示验收失败**无限重试**。这一点必须配合 `-max-elapsed` 与 `-max-stall`：没有闸门时，一个永远无法通过验收的叶子会一直重试下去持续烧 token。超时的叶子直接判 `FAILED` 并写明原因，**不会**触发重新拆解——时间不够不代表任务太大，重拆只会重置时钟；但**停滞**（重复同一失败）的叶子会触发重新拆解，那才是"任务太大/方向不对"的信号。

---

## 核心理念

现有编程 Agent 极度依赖旗舰长上下文模型。小模型上常见：

* **上下文爆炸**：历史越长，约束被冲掉，幻觉增多。
* **推理过载**：一次规划过多细节，逻辑崩溃。

本引擎不追求单次规划的聪明，而用工程约束保证能把复杂目标做完：

1. **树状递归拆解** — 复合节点当架构师：划边界、写接口契约、写 Definition of Done。
2. **契约先于执行** — 没有输入/输出约束和可运行验收命令，不下发叶子。
3. **叶子隔离上下文** — 每次执行新建对话；失败重试也是新对话，只附带错误输出。

---

## 模块

| 包 | 职责 |
|---|---|
| `pkg/models` | 任务节点、状态机、契约、DoD |
| `pkg/engine` | 任务树、调度（依赖/就绪/冒泡）、原子 JSON 持久化 |
| `pkg/llm` | OpenAI 兼容客户端、流式、重试、弱模型 JSON 容错解析 |
| `pkg/cost` | 模型价目表、token→美元成本估算 |
| `pkg/tools` | 工作区沙箱工具 |
| `pkg/agent` | Decomposer、Worker、Verifier、Orchestrator、预算护栏 |
| `cmd/divvy` | CLI |

状态：`PENDING` → `DECOMPOSING` / `RUNNING` → `VERIFYING` → `COMPLETED` / `FAILED`。

架构、并发与隔离的两道防线、失败恢复矩阵、扩展点（加工具 / 改 prompt / 加语言）见 **[docs/architecture.md](docs/architecture.md)**。

---

## 开发

```bash
go test ./...            # 全量测试
go test -race ./...      # 并发路径
golangci-lint run ./...  # 静态检查
```

端到端单测使用脚本化 Mock LLM，不访问网络；会在临时目录里真正 `go test` 验收生成的包。CI 跑四个 job：常规测试（含 gofmt）、`-race`、`golangci-lint`、以及装了 Chrome 的浏览器 job。

当前语句覆盖率约 80%（`models` 100% / `cost` 95% / `engine` 95% / `llm` 92% / `agent` 79% / `tools` 77% / `cmd` 66%）。写 `cmd` 层的测试时注意：**不要用需要真实 LLM 的模式**（如 `-plan` 打到不存在的 endpoint）——`llm` 客户端默认无限重试，会让测试挂死；这类路径请用 `httptest` 或直接构造持久化产物。

---

## Roadmap

- [x] 阶段 1：任务树核心引擎、持久化、调度器
- [x] 阶段 2：LLM 接入、沙箱工具、弱模型 JSON 解析
- [x] 阶段 3：Decomposer 与契约生成
- [x] 阶段 4：隔离 Worker、Verifier、失败再拆
- [x] 阶段 5：CLI、断点续跑、树状进度、mock 端到端
- [x] 阶段 6：并行叶子执行（`-parallel`）、token 用量统计、plan-only 规划（`-plan`）
- [x] 阶段 7：叶子级工作区隔离（`-isolate`：快照 → 隔离执行 → 验收后合并/失败回滚）
- [x] 阶段 8：按节点 token 用量持久化（resume 累计）、plan 契约完整性检查
- [x] 阶段 9：执行审计与成本可见性（`-git-commit` 每叶子一提交、`-plan -strict`、树视图 token 显示）
- [x] 阶段 10：成本估算与预算护栏（内置/自定义价目表、节点级美元成本、`-max-cost`/`-budget-tokens` 会话级硬上限）
- [x] 阶段 11：代码内容搜索（`search_files` 正则定位、glob 过滤）与会话列表（`-sessions`）
- [x] 阶段 12：集成叶子与根目标验收（强制装配入口、端到端验收通过才 COMPLETED、失败回路由）
- [x] 阶段 13：交互式 REPL（工头对话层 + 独立上下文叶子执行、只保留多轮压缩摘要、`/help` `/clear` `/status` `/exit`）
- [x] 阶段 14：引导式工作流（计划评审与 `/approve` 敲定、执行中 `/add` `/redo` `/pause`、agent `ask` 提问、可 resume）
- [x] 阶段 15：多语言项目验收（自动探测 Go/Node/Rust/Python/Makefile，按栈生成叶子与根 DoD，模型显式命令优先）
- [x] 阶段 16：联网搜索与抓取（`-web` 开启 web_search/web_fetch、HTML 转文本、可配 DuckDuckGo/SearXNG、非公开地址与端口拦截）
- [x] 阶段 17：扩展工具集（`-browser` headless Chrome、http_request、只读 git_status/diff/log、Go AST find_symbol、json_query）
- [x] 阶段 18：原子多点编辑与文件原语（replace_lines 的 edits 批量、delete_path、move_path）、Go 验收失败诊断块前置
- [x] 阶段 19：引用查找（find_symbol references 文本级用法扫描）与 finish 前确定性自检 review_diff（冲突标记/调试残留/密钥/大 diff）
- [x] 阶段 20：二进制下载 download_file（`-web` 门控、SSRF 防护复用、20MB 上限、失败清理）与只读 sqlite_query（纯 Go modernc 驱动、mode=ro/query_only、结果集压缩）
- [x] 阶段 21：并发正确性收尾——浏览器叶子级租约（-parallel 下多步流程不再跨叶子串台、可取消等待）与 finish 自动门控（覆盖未跟踪新文件，冲突标记/密钥硬拦截并回灌、debug/大 diff 仅告警）
- [x] 阶段 22：少轮次与全栈诊断——read_file 的 paths 批量读（最多 8 个文件/次）、outline 顶层声明地图（Go AST 签名、其它语言模式回退）、诊断压缩扩展到 Python/Rust/Node
- [x] 阶段 23：并发正确性收口——`trimHistory` 对齐 tool_call 边界（`-native-tools` 不再打出孤立 tool 消息）、镜像合并按快照摘要在冲突时放弃而非覆盖、声明相同产出的叶子调度互斥、契约体检接入 `Run` 路径
- [x] 阶段 24：重试时间闸门（`-max-elapsed`，单叶子跨尝试的墙钟预算，堵住 `-max-retries 0` 的无限烧钱）、`Clone` 深拷贝修正（`ErrorHistory` 不再共享底层数组）
- [x] 阶段 25：工程基线——CI 增加 `-race`、`golangci-lint`、gofmt 检查与浏览器 job；`pkg/models` 补齐单测（0% → 100%）；新增 `docs/architecture.md`
- [x] 阶段 26：覆盖与健壮性收口——总覆盖率 71% → 80%（`llm` 57%→92%、`engine` 62%→95%、`cmd` 48%→66%）；`UpdateNode` 改为原子写入（回调失败不再留下半改状态）；诊断规则补齐 Makefile 与通用工作区（此前这两类工作区失败时**完全不压缩**，24KB 原始日志整个进 prompt）
- [x] 阶段 27：停滞检测与失败语义收口——失败指纹（复用根因诊断行、数字掩码）与 `-max-stall` 让默认无限重试真正收敛；步数耗尽不再伪装成功并补上步数预算提醒；`-report` 会话复盘与 `-prune` 旧会话清理

弱模型上的 Prompt 与拆分粒度仍需按具体模型微调（`-max-depth`、`-max-steps`、`-extra`）。

---

## 已知限制

如实记录当前版本的边界，避免踩坑：

- **`-parallel` 只在叶子产出互不重叠时才是完全并行的。** 声明了相同产出的叶子会被强制串行（这是正确的，但会牺牲并行度）；契约里**没写**产出的叶子无法预判，只能靠合并时的冲突检测兜底——那种情况会多一次重试。
- **契约质量决定并行度与自愈能力。** 弱模型常常不填 `contract.outputs`，此时引擎既无法提前串行化冲突叶子，也拿不到"这个叶子该产出什么"的约束。`-plan` 的人工检查比事后补救便宜得多。
- **`-native-tools` 与 JSON 动作模式的成熟度不同。** 默认的 JSON 动作模式是给弱模型准备的；`-native-tools` 走 OpenAI `tool_calls`，历史裁剪已做配对对齐，但对同样本要求更高的模型。
- **`find_symbol` 的 `references` 是文本级扫描**，不做类型分析：注释和字符串里的同名提及也会出现，别名与动态调用会漏掉。Go 的**定义**定位走真实 AST，是准确的。
- **验收命令由模型生成，默认值只是兜底。** 根目标验收的兜底命令（各栈的 build / `npm test`）远弱于模型显式给出的端到端命令（启动服务 + `curl -f`）。若计划里没有集成叶子，根会直接 `FAILED` 而不是假装完成。
- **多语言诊断按语言的规则表抽取**（Go/Python/Rust/Node 各自的专用规则，外加 Makefile 与通用工作区的兜底规则），新语言需要在 `pkg/tools/diagnostics.go` 的 `diagRules` 里加一条；未覆盖的工具链会退回通用规则，不会完全没有压缩。
- **`-max-elapsed` 是单叶子预算，不是全局预算。** 全局请用 `-max-cost` / `-budget-tokens`。
- **`-max-stall` 靠指纹比对，太宽的指纹会误伤。** 指纹优先取根因诊断行（已去重、已剥离易变细节），没有诊断规则时才回退到"输出头部 + 数字掩码"——这意味着只有数字变化的失败会被视为同一个。若某个工具链的输出结构特殊导致误判，可以 `-max-stall 0` 关掉，只用 `-max-elapsed` 兜底。
- **`-prune` 会真删文件**（会话 JSON + 日志 + 事件流），只保留 `-keep` 个最近的会话。它永不删 `LATEST` 指向的会话，但先跑一次 `-prune -dry-run` 看看清单永远是值得的。
- **浏览器工具需要本机 Chrome/Chromium**，且 `-parallel` 下多个叶子共享一个标签页（靠叶子级租约保证多步流程不串台），因此浏览器步骤整体是串行的。
