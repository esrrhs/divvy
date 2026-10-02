# go_llm_engine

面向弱模型（小参数、低算力、廉价模型）的自动化编程 Agent。  
核心策略：**愚公移山，分而治之** —— 把大目标拆成带契约和验收命令的任务树，每个叶子在隔离上下文里执行，靠编译/测试而不是长会话记忆交付结果。

---

## 快速开始

```bash
go install github.com/esrrhs/go_llm_engine/cmd/engine@latest
# 或在仓库内：
go build -o go_llm_engine ./cmd/engine
```

任意 **OpenAI 兼容** 接口都可以，包括 OpenAI、vLLM、Ollama、本地网关。本地 Ollama 实战配置见 [docs/qwen3.8-local.md](docs/qwen3.8-local.md)、[docs/debug-local-ollama.md](docs/debug-local-ollama.md)：

```bash
export OPENAI_API_KEY=sk-...
export OPENAI_BASE_URL=http://127.0.0.1:11434/v1   # 本地模型带 /v1
export OPENAI_MODEL=qwen2.5-coder:14b

./go_llm_engine -workdir ./ws "用 Go 写一个 /health 返回 ok 的 HTTP 服务，并带单测"
```

先只拆解不执行，人工检查任务树后再跑：

```bash
./go_llm_engine -plan -workdir ./ws "目标"   # 生成任务树后退出
./go_llm_engine -resume -workdir ./ws        # 执行已规划的任务
```

常用参数：

| 参数 | 含义 |
|---|---|
| `-workdir` | 代码落地目录（工具只能读写这里） |
| `-resume` | 从上次会话继续（默认读 `.go_llm_engine/LATEST`） |
| `-session` | 指定会话 ID |
| `-status` | 只打印任务树，不执行 |
| `-plan` | 只拆解出任务树并保存，不执行（配合 `-resume` 使用） |
| `-strict` | 配合 `-plan`：有契约告警时以非零码退出（CI 自动把关） |
| `-git-commit` | 每个叶子合并后自动 `git commit`（workdir 需为 git 仓库），哈希记入叶子摘要 |
| `-parallel` | 同时执行的叶子数，默认 `1`；>1 时自动开启 `-isolate` |
| `-isolate` | 叶子在主工作区的临时镜像里执行，验收通过才合并回主工作区，失败即丢弃 |
| `-max-retries` | 叶子验收失败最多重试几次，`0`（默认）为无限 |
| `-retry-max-wait` | 指数退避上限，默认 `30s` |
| `-native-tools` | 改用 OpenAI `tool_calls`（强模型可开；弱模型默认 JSON 更稳） |
| `-extra` | 合并进请求体的 JSON，例如 Qwen3：`'{"enable_thinking":false}'` |
| `-max-cost` | 会话成本上限（美元），含 resume 之前的花费；超限即停并保存，`0` 为不限 |
| `-budget-tokens` | 会话 token 上限，含 resume 之前的花费；超限即停并保存，`0` 为不限 |
| `-pricing` | 自定义价目表：JSON 文本或 JSON 文件路径，如 `'{"my-model":{"input":0.15,"output":0.6}}'`（每百万 token 美元价） |
| `-sessions` | 列出已保存的会话（状态、叶子进度、目标），不执行 |
| `-v` | 打印模型原文和工具输出（结束时附带分项 token 用量） |

中断（Ctrl+C）会保存任务树，之后：

```bash
./go_llm_engine -resume -workdir ./ws
```

---

## 运行时在做什么

```
根目标
  └─ Decomposer 输出 JSON（原子？或 2~6 个子任务 + 契约 + 验收命令）
        └─ 叶子 Worker：干净上下文 + 6 个工具
              list_dir / read_file / write_file / replace_lines / run_bash / search_files
              └─ Verifier 跑 DoD 命令（如 go test ./...）
                    ├─ 通过 → 向上冒泡 COMPLETED
                    └─ 失败 → 新的隔离上下文重试（带上错误，指数退避，上限 30s，默认无限次）
```

叶子执行**不携带**其它叶子的对话历史，只注入：当前任务、契约、父节点/依赖摘要、少量相关文件、验收命令。
`search_files` 用正则搜索文件内容并返回紧凑的 `相对路径:行号:匹配行`：修改现有代码时，弱模型用它一次定位符号，不必逐个读整个文件，省步骤也省上下文（自动跳过 `.git` 等目录，可用 `glob` 过滤，默认忽略大小写）。
没有依赖关系的就绪叶子可以并发执行（`-parallel N`，默认 `1`）；每次 LLM 调用的 token 用量按节点记入任务树并随会话持久化，运行结束打印本次与会话累计（resume 后自动累加），树状进度与 `-status` 里也会显示每个节点的消耗。

开启隔离（`-isolate`，`-parallel >1` 时自动生效）后，叶子在主工作区的**临时镜像副本**里写代码、跑验收命令：
验收通过才把新增/修改的文件合并回主工作区（删除也会传播），失败或中断则整个镜像丢弃——失败的尝试永远不会污染共享工作区或兄弟叶子。

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

默认完全离线；加上 `-web` 后叶子获得两个网络工具，用于查询模型自身无法获知的最新文档/版本：

- `web_search`：`{"query":"...","max_results":5}` → 编号结果（标题/URL/摘要）。
- `web_fetch`：`{"url":"https://..."}` → 抓取单个页面，**HTML 自动转成纯文本**（剥离 script/style），文本/JSON/XML 原样返回。

```bash
./go_llm_engine -web -workdir ./ws "查一下 X 的最新 API，写一个调用示例"
```

搜索后端可通过 `-search-url` 配置（模板必须含 `{query}`）：

- 留空：内置 **DuckDuckGo lite**，无需 API Key（仅解析公开 HTML，可能被网络策略拦截）。
- **SearXNG**：`-search-url 'http://host/search?q={query}&format=json'`，走 JSON，适合自建稳定检索。

安全约束（web_fetch 与每一跳重定向都会执行）：只允许 http/https；目标域名解析后若指向回环/私网/链路本地/CGNAT 等非公开地址一律拒绝；只允许 80/443；重定向到内网同样拦截。因此 agent 无法借抓取访问本机或内网服务。

### 浏览器、HTTP、Git、代码理解工具

除网络搜索外，工具集还包含以下能力（按开关/环境可用）：

**Headless 浏览器（`-browser`，需 Chrome/Chromium）**

- `browser_navigate`：加载 **JS 渲染页面**，返回标题 + 渲染后文本（补 web_fetch 拿不到 SPA 内容的缺口）；
- `browser_click`（CSS 选择器）、`browser_type`（输入文本，可先清空）、`browser_text`（读取元素/正文渲染文本）；
- `browser_screenshot`：整页 **PNG** 存入工作区（`{"path":"shot.png"}`）。
- 浏览器进程懒启动、跨叶子复用同一标签页；用户中断时才关闭。这也让 Web 前端交付物具备"真实浏览器交互"级别的验收手段。

**结构化 HTTP（`-web` 下）**

- `http_request`：`{"url","method","headers","body"}`，返回状态码/响应头/正文；弱模型不必在 shell 里手拼 curl。

**Git 只读工具（工作区为 git 仓库时）**

- `git_status`、`git_diff`（可选 `staged`/`path`）、`git_log`（可选 `limit`/`path`）；均为只读，不改动仓库。

**代码/数据理解（默认可用）**

- `find_symbol`：按名字定位定义——**Go 走真实 AST**（func/type/var 精确到行），其它语言回退正则；
- `json_query`：用点号/方括号路径（`items.0.name`、`items[0].name`）从大 JSON 里抽单个值，避免整文件进上下文。

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
go build -o go_llm_engine ./cmd/engine
./go_llm_engine -interactive \
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
./go_llm_engine -guided -isolate -git-commit \
  -base-url http://127.0.0.1:11434/v1 -model qwen3.8:27b -workdir ./ws "你的目标"
```

`-plan` 在拆解完成后会做完整性检查并对弱契约告警：无验收命令、验收只有占位符（`ls`）、无产出声明、兄弟叶子声明了相同产出文件。

### 成本估算与预算护栏

引擎内置常见 OpenAI 模型的近似价目（每百万 token 美元价），用 `-pricing` 可覆盖或追加自有/本地模型的价格（JSON 文本或文件路径，键名同时支持精确匹配和最长子串匹配）。

- 每次调用的成本按节点随 token 用量一起显示：树视图节点标签、结束/`-status` 用量汇总（本次 + 会话累计）。
- `-max-cost`（美元）与 `-budget-tokens` 是**会话级硬上限**，计入 resume 之前已持久化的花费；超限立即取消运行、把在途节点复位为 `PENDING` 并保存任务树。提高上限后用 `-resume` 继续即可，不会重试或重复烧钱。
- 模型在价目表中无对应价格时不显示估算（本地零成本模型的典型情况），token 预算仍然生效。

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
| `cmd/engine` | CLI |

状态：`PENDING` → `DECOMPOSING` / `RUNNING` → `VERIFYING` → `COMPLETED` / `FAILED`。

---

## 开发

```bash
go test ./...
```

端到端单测使用脚本化 Mock LLM，不访问网络；会在临时目录里真正 `go test` 验收生成的包。

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

弱模型上的 Prompt 与拆分粒度仍需按具体模型微调（`-max-depth`、`-max-steps`、`-extra`）。
