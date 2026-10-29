# divvy

[<img src="https://img.shields.io/github/license/esrrhs/divvy">](https://github.com/esrrhs/divvy)
[<img src="https://img.shields.io/github/languages/top/esrrhs/divvy">](https://github.com/esrrhs/divvy)
[<img src="https://img.shields.io/github/go-mod/go-version/esrrhs/divvy">](https://github.com/esrrhs/divvy)
[<img src="https://img.shields.io/github/actions/workflow/status/esrrhs/divvy/test.yml?branch=master">](https://github.com/esrrhs/divvy/actions)
[![codecov](https://codecov.io/gh/esrrhs/divvy/graph/badge.svg)](https://codecov.io/gh/esrrhs/divvy)

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
| `-wave` | 一波的配额：最多允许多少个"已规划未执行"的叶子、一波最多发起多少个叶子，默认 `3`。它同时节流拆解与执行——只节流拆解是不够的，弱模型会一次把目标拆成所有叶子 |
| `-aggregate` | 每波结束对整个工作区跑的聚合检查命令，可重复或用 `;` 分隔；默认按工作区技术栈推断（Go：`go build ./...` + `go test ./...`，两条分开跑） |
| `-max-aggregate-fails` | 聚合连续失败多少次后放弃，默认 `4`（`0` = 不限） |
| `-solo` | 整棵树退化为一个叶子：单上下文跑完整个目标，验收失败在同一对话里回喂继续修（目标能装进一个上下文时首选） |
| `-solo-steps` | 配合 `-solo`：每轮工具调用预算，默认 `80` |
| `-solo-rounds` | 配合 `-solo`：验收失败后的修复轮数，默认 `3` |
| `-solo-context` | 配合 `-solo`：每次请求携带的对话字符上限，默认 `100000`（完整历史留在内存，只压缩窗口） |
| `-dod` | 显式验收命令，可重复，单个值内也可用 `;` 分隔。覆盖规划器与工具链推断出的默认验收（树模式只作用于根节点，`-solo` 下作用于整次运行） |
| `-strict` | 配合 `-plan`：有契约告警或工作区布局告警时以非零码退出（CI 自动把关） |
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
                ┌──────────────────────────────────────────────┐
                │                                              │
                ▼                                              │
  ① 拆分一波  Decomposer 输出 JSON（原子？或 2~6 个子任务）      │
        │        只展开到"未执行叶子 ≤ -wave"为止，不拆到底      │
        ▼                                                     │
  ② 执行这一波（最多 -wave 个叶子，跑完就聚合，不跑完再拆）        │
        └─ 叶子 Worker：干净上下文 + 9 个基础工具                 │
              list_dir / read_file / write_file / replace_lines / delete_path
              / move_path / run_bash / search_files / find_files
              └─ Verifier 跑叶子 DoD（如 go test ./pkg/...）
                    ├─ 通过 → 向上冒泡 COMPLETED
                    └─ 失败 → 新的隔离上下文重试（带错误，指数退避，默认无限次）
        │                                                     │
        ▼                                                     │
  ③ 聚合检查  整个工作区 build + test（不是某个叶子自己的 DoD）     │
        ├─ 失败 → 插一个"聚合修复"叶子（带失败输出 + 本波清单）────┤
        └─ 通过 → 还有活？ ──── 是 ─────────────────────────────┘
                     └── 否 → ④ 根目标验收（真跑交付物）→ 完成
```

**关键区别：这不是"开局拆一把梭，最后总验收一次"。** 拆解、执行、聚合是一个**循环**，每一圈都以"整个工程还能编译能测试"收尾。详见[循环式推进](#循环式推进wave--aggregate--replan)。

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
- **布局层（前置）**：符号链接让"同一个文件"拥有两个路径。副本镜像不携带链接（快照跳过非常规文件），于是一个叶子在镜像里新建 `core/x.go`、另一个叶子在真实目录 `internal/core/x.go` 里工作，合并时这是两个不同路径；而合并一旦穿过链接，写入会落到链接指向处而非待合并路径。所以启动时先跑一次**工作区布局检查**（只读目录项，不读源码、不跟随链接），把符号链接（指向工作区内 / 工作区外 / 悬空）和"`go.mod` 模块路径与目录层级差一层"（模块 `app` 的包却放在 `app/` 下）在**任何叶子启动前**以 `layout check:` 告警列出。合并阶段真撞上穿过链接的路径时返回 `MirrorLayoutError` 并直接判失败：那不是并发冲突，重新快照解决不了任何问题，只会空转。

### 集成叶子与根目标验收

叶子 DoD 是局部的（某包 `go build`/`go test`），全部叶子通过**不等于**用户目标达成——实测中曾出现：所有包编译、测试通过，却漏掉 `main.go`，整个东西根本不能运行。循环式推进把这件事从"最后一次性算总账"变成了"每波一次"，但最后一道关仍然在：

- **集成叶子**：当目标是可运行的程序/服务/API/CLI 时，拆解提示要求最后一个 `integrate` 叶子负责组装、创建入口（`main.go`），并依赖所有其他叶子。这是 prompt 层面的要求，模型可能忽略，因此引擎另有兜底（下一条）。
- **根目标验收**：复合根在所有叶子完成后，再跑一次根级 DoD——必须**真正运行交付物并端到端验证**（启动服务、用 `curl -f` 打真实接口、再停掉；CLI 则用真实参数执行），而不只是编译某个包。验收通过（`IntegrationVerified`）根才允许 `COMPLETED`。
- **计划期结构校验**：根验收命令里写死的路径（`go run ./cmd/demo`、`./bin/app`、`python app.py`）必须由某个叶子的产出覆盖。没有覆盖的，不会等叶子全部跑完才发现——规划期就判定计划不完整，带着具体缺陷回退重拆（受 `-max-redecompose` 约束）；重拆预算用尽仍缺，则**自动补一个 `integrate_entry` 叶子**，它产出验收实际运行的入口文件、依赖所有已有子节点。整包级别的命令（`go build ./...`、`go test ./...`）不参与该检查：它们描述范围而非某个必须存在的文件。
- 验收失败会把错误**回路由给集成叶子**，它带着错误重新执行并修复装配，指数退避、受 `-max-retries` 约束。集成叶子先找根的直接子节点，其次在整棵树里找——装配工作被规划到某个 compound 之下时，依然能找到。
- 老会话用 `-resume` 续跑时，若根已是 `FAILED` 且缺集成叶子，启动时会自动补叶子并把根置回 `RUNNING`。否则失败会话会退化成"每次启动打印一遍树然后 exit 1"的死循环——配合 systemd 的 `Restart=on-failure` 就是无限空转。
- `-plan`/`-strict` 会对较轻的缺口告警：可运行目标却无入口叶子、无根级验收命令；工作区布局有符号链接或模块路径与目录层级不一致（见上文"布局层"），`-strict` 下同样以非零码退出。

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

安全约束（web_fetch、download_file 与每一跳重定向都会执行）：只允许 http/https；目标域名解析后若指向回环/私网/链路本地/CGNAT/保留段（含 NAT64、TEST-NET、240.0.0.0/4 等非全局可路由范围）一律拒绝；只允许 80/443；重定向到内网同样拦截。因此 agent 无法借抓取或下载访问本机或内网服务。

策略在 **dial 时**强制执行：transport 的 DialContext 在建立连接的瞬间重新解析主机、校验地址、并直连其中一个是校验过的 IP。只在请求前校验 URL 是不够的——重绑定服务器可以让校验时解析到公网 IP、几秒后实际连接解析到内网 IP（经典 DNS rebinding TOCTOU）；把检查放进 dialer 才保证"通过校验的地址就是被连接的地址"。这一约束覆盖初始请求和全部重定向跳转。`-search-url` 配置的检索端点是操作者显式信任的（自建 SearXNG 常在内网），与 `AllowLocal` 同级，不走该策略。

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

### 桌面应用（Tauri，macOS / Linux / Windows）

`desktop/` 是 Tauri v2 外壳：启动时按平台定位并拉起内置的 `divvy -serve` sidecar（externalBin），解析其启动日志里的随机端口与一次性 token，再把 WebView 指向 sidecar（发布模式）或 Vite（开发模式，`/api` 代理到 sidecar）。数据与默认工作区放在平台应用数据目录（macOS：`~/Library/Application Support/com.divvy.desktop/`；Linux：`~/.local/share/com.divvy.desktop/`；Windows：`%APPDATA%\com.divvy.desktop`）。

进程管理按平台实现：POSIX 上 sidecar 运行在独立进程组，关窗先发 SIGINT 让运行中的会话 checkpoint，等待 12s 兜底后再 SIGKILL 整个进程组，不留僵尸进程；Windows 上 GUI 程序没有控制台信号通道，关窗即 `taskkill /F /T` 结束整棵进程树（叶子工具子进程一并清理），会话在下次启动时从最近一次 checkpoint 续跑。sidecar 启动或 URL/token 解析失败会弹原生错误框并以非零码退出。

前置条件：

```bash
# macOS
xcode-select --install                 # Xcode 命令行工具
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh   # Rust
# 另需 Go（构建 sidecar）与 Node.js（构建前端/运行 Tauri 钩子）

# Ubuntu/Debian（Tauri Linux 依赖）
sudo apt-get install -y libwebkit2gtk-4.1-dev libayatana-appindicator3-dev \
  librsvg2-dev libssl-dev libxdo-dev file
# 其余同 macOS：Rust（rustup）、Go、Node.js

# Windows
# - Rust（rustup，默认 msvc 工具链与 Visual Studio Build Tools）
# - WebView2 Runtime（Win11 自带）
# - Go、Node.js
# - Git for Windows：叶子的 shell 工具按 sh 方言执行，需要 sh.exe 在 PATH
```

开发模式（热更新前端；首次会先 `go build` sidecar 再拉起 Vite；跨平台钩子由 Node 实现，不依赖 POSIX shell）：

```bash
cd desktop
npm ci
npm run dev          # = tauri dev；自动构建 binaries/divvy-sidecar-<triple>
```

发布构建（构建前端 → 构建 sidecar → 按当前平台打包：macOS 为 `.app`/`.dmg`，Linux 为 `.deb`/AppImage，Windows 为 NSIS 安装包）：

```bash
cd desktop && npm run build
# macOS 产物：target/release/bundle/macos/divvy.app、target/release/bundle/dmg/*.dmg
# Linux 产物：target/release/bundle/deb/*.deb、target/release/bundle/appimage/*.AppImage
# Windows 产物：target/release/bundle/nsis/*.exe
```

sidecar 二进制按 `$GOOS/$GOARCH` 映射命名（`binaries/divvy-sidecar-aarch64-apple-darwin`、`divvy-sidecar-x86_64-pc-windows-msvc.exe` 等，支持 darwin/linux/windows × amd64/arm64 六个目标），可单独运行 `node desktop/scripts/build-sidecar.mjs` 刷新（设置 `GOOS/GOARCH` 即可交叉构建）。排障可用环境变量覆盖：`DIVVY_SIDECAR`（替换后端程序路径）、`DIVVY_SIDECAR_PORT`、`DIVVY_SIDECAR_STARTUP_TIMEOUT_MS`。

### 成本估算与预算护栏

引擎内置常见 OpenAI 模型的近似价目（每百万 token 美元价），用 `-pricing` 可覆盖或追加自有/本地模型的价格（JSON 文本或文件路径，键名同时支持精确匹配和最长子串匹配）。

- 每次调用的成本按节点随 token 用量一起显示：树视图节点标签、结束/`-status` 用量汇总（本次 + 会话累计）。
- `-max-cost`（美元）与 `-budget-tokens` 是**会话级硬上限**，计入 resume 之前已持久化的花费；超限立即取消运行、把在途节点复位为 `PENDING` 并保存任务树。提高上限后用 `-resume` 继续即可，不会重试或重复烧钱。
- 模型在价目表中无对应价格时不显示估算（本地零成本模型的典型情况），token 预算仍然生效。

四层闸门互相独立：会话级 `-max-cost` / `-budget-tokens` 管总量；单叶子 `-max-elapsed`（默认 `45m`）管"一个慢叶子"；`-max-stall`（默认 `3`）管"一个快但在原地打转的叶子"；`-max-steps` 管单次尝试的收尾。

> `-max-retries 0`（默认）表示验收失败**无限重试**。这一点必须配合 `-max-elapsed` 与 `-max-stall`：没有闸门时，一个永远无法通过验收的叶子会一直重试下去持续烧 token。超时的叶子直接判 `FAILED` 并写明原因，**不会**触发重新拆解——时间不够不代表任务太大，重拆只会重置时钟；但**停滞**（重复同一失败）的叶子会触发重新拆解，那才是"任务太大/方向不对"的信号。

### 单上下文模式（`-solo`）：什么时候不该拆

拆解不是免费的。它换来的好处是"每个叶子只看见一小块"，代价是**全局性质没有归属**：叶子契约只能写"我产出哪些文件"，写不了"整个工程不许有第三方依赖"或"这五个测试场景必须存在"。于是每个叶子都能完美履约，合起来仍然违反目标——实测中一个 66 叶子的运行花了 17.8M token，交付物里却带着目标明令禁止的第三方测试依赖。

`-solo` 走另一条路：整棵树退化为一个叶子，一个上下文、一个循环，**验收失败回到同一个对话里继续修**。历史留在内存，只有窗口按 `-solo-context` 压缩，被裁掉的中间步骤用一份确定性日志（写过哪些文件、跑过哪些命令、最近失败什么）代替，不额外花一次模型调用去总结。

判断标准很简单：**目标大概能装进一个上下文窗口，就用 `-solo`；装不下（或需要并发推进互不相干的模块）才用树。** 同一个 phys2d 目标（纯 Go 二维物理引擎，7 个包、1000 行）两种模式都跑过，数字见下。

| | 树模式 | `-solo` |
|---|---|---|
| 模型调用 | 5664 | 103（首轮）/ 78（修复轮） |
| token | 17.8M | 2.2M（首轮）/ 2.0M（修复轮） |
| 墙钟 | 4h22m | 36m（首轮） |
| 违反"只能用标准库" | 是（引入了 testify） | 否 |
| 目标要求的 5 个测试场景 | 缺 3 个 | 5 个全有 + 表驱动 |
| 路径踩坑（`phys2d/` 双嵌套） | 跨叶子传播，第 5 次尝试合并冲突才暴露 | 同一上下文里 3 步自纠（`ls` → `mv` → `rmdir`） |

五件事决定了这套模式是否可用，都在 `pkg/agent/solo.go` 里：

**验收不是猜的。** 空工作区里推断出的验收只能是"有文件就算过"这种占位检查，跑过了也证明不了什么——第一次实跑就是这样"成功"的，模型自己都在对话里说 `TestCircleFallsToGround` 还在失败。所以验收有两道修正：`refreshAcceptance` 在每轮开始和验收前重新解析（`go.mod` 一出现就升级成 `go test ./...`，并明确告知模型）；再不够就用 `-dod` 把验收钉死。运算符给的验收是契约，不会被重新解析。

**同一失败不重撞。** 只回喂错误输出，第二次修复往往还是同一个猜想的第二次尝试。实测那次穿透深度从 `0.0985` 变成 `-0.3973`——改的是同一处代码，更糟了。现在同一指纹（`stallFingerprint`，数字被归一化，所以"深一点/深两点"算同一失败）连续出现两次就换提示词：要求先说出假设、加打印或最小复现去观测、观测推翻了假设就换假设，而不是调数字。到 `-max-stall` 次直接提前收手，报 `stalled` 而不是把轮数耗完。

这一层在两个粒度上都生效：**轮之间**换修复策略，**一轮之内**同样打断。后者是实跑逼出来的——一轮有 60 步，模型可以在轮边界之前就把步数耗在"改一处、跑一次"的循环里（实测第 38 步仍在同一个断言上打转，数值在 `-0.15` 和 `-1953` 之间来回）。同一命令连续两次以相同输出失败，就直接往对话里插一条干预，并且明确要求把临时观测程序放在**工作区内**（放在 `/tmp` 的程序导入不了工程自己的包，跑不起来）。

**对话本身是状态。** 树模式的进度在节点里，`-resume` 读回来就能接着跑；solo 的进度在对话里，丢了就等于失忆。所以转录按 JSONL 落在 `<datadir>/<session>.solo.jsonl`（每 10 步与每轮结束落盘，崩溃最多丢最后一行），`-resume` 会接着这段对话继续，并把当前工作区重新述说一遍（首轮那份快照已经过期）。已完成的 solo 会话再 `-resume` 是空操作，不会把已经通过验收的交付物重做一遍。

**工作区非空时先说清楚。** 实测中把原始目标喂给一个已经有可用实现的工作区，模型按目标里"从零实现"的字面意思把每个文件全部重写了一遍，扔掉了已经通过测试的代码。所以提示词在工作区非空时会明确：这里不是空白开始，先跑验收命令看现在到哪了，读一个文件再替换它，改错的部分、留对的部分。

**验收过了不等于目标做完了。** 这是这套模式唯一无法用命令覆盖的部分：`go test ./...` 绿了，但目标要求的"向量运算用表驱动测试"没有（全是一个函数一个用例、没有 `t.Run`），"每 0.2 秒打印一次、模拟 2 秒"的 demo 实际打印 120 次、跑 24 秒。验收命令通过，两条要求没做到。所以验收首次通过后会**再花一次调用**做收尾审计：逐条对照目标要证据（哪个文件、哪个测试函数、哪次命令输出），对不上就现在做。审计只做一次，不占轮流预算（有一轮额外的量），也不会把已经通过的运行拖成死循环。

---

## 循环式推进（wave → aggregate → replan）

> 把"一次性拆完整棵树"换成"拆一小波 → 跑 → 聚合 → 再拆"的循环。这是 divvy 现在的主循环，不是提案。

### 问题：开局拆一把梭，最后拼不回来

现在的流程是**开环**的：分解器在拿到任何执行事实之前，就要把整个目标想清楚、一次拆到叶子；然后所有叶子各跑各的；直到最后一个叶子落地，才第一次、也是唯一一次把整个工程拼起来跑。

实测代价（66 叶子 / 17.8M token / 4h22m）：每片叶子都拿到了 COMPLETED，根目标验收却失败，而且失败得**无法归因**——错误是"`go run ./cmd/demo` 跑不起来"，可那时 66 个上下文都已经销毁，没人知道自己留了什么坑。更早的一次是 1040 行代码里带进了目标明令禁止的第三方依赖：每个叶子的契约都写着"我产出这些文件"，没有一条契约能表达"整个工程不许有第三方依赖"。

把这三件事连起来看，病根不是"拆解"本身，而是**反馈太晚、太大、太笼统**：

1. 拆解发生在现实之前。规划器看不到编译器会怎么抱怨、包该怎么分层，只能凭目标文字想象，想象错了也没有任何东西会纠正它。
2. 集成只发生一次。中间十几步里，交付物一直处于"可能根本编不过"的状态，但没有任何检查在跑。
3. 失败发生在最后。等到发现时，能改的只有"再来一次全局装配"，而不是"刚才那步弄坏了 `collision` 包"。

### 设计：三件事一轮，循环推进

**① 拆分（wave）** — 只展开到"已规划未执行"的叶子够跑一波为止。`-wave 3` 意味着：手里最多攥着 3 个还没跑的叶子。拆完一波就停手，等现实回来。

这样做的直接后果是**拆解变成增量的**：第二波之后的每次拆解，看到的工作区快照里已经有上一波写下的真实代码，拆出来的子任务自然贴着现实，而不是贴着想象。

**② 执行（execute）** — 这一波的叶子照旧：隔离上下文、跑各自 DoD、失败重试、成功合并。但**一波最多只发起 `-wave` 个叶子**，跑完就聚合，剩下的留给下一波。

这一条是被真跑逼出来的：第一次实现只限制了"拆解"，结果根节点一次就吐出全部 6 个叶子——节流等于没生效，第一个聚合仍然在 6 个叶子全跑完之后。**只节流拆解、不节流执行，等于没有节流**，因为弱模型最常见的做法就是把目标一把拆成所有叶子。所以配额同时管两件事：一手能规划几个，一手能跑几个。

**③ 聚合（aggregate）** — 一波全部落地后，对**整个工作区**跑一次编译+测试：

```
go build ./... && go test ./...                # Go，默认（两条命令分开跑）
npm test --if-present                          # Node
cargo build && cargo test                      # Rust
python3 -m py_compile $(find . -name '*.py')   # Python
```

Go 的 build 和 test 是**两条独立的命令**：编译失败和测试失败是两类问题，合并成一条后 `&&` 只会报告前者。

这不是根目标验收。它只回答一个问题：**"现在的工程还站得住吗？"**——不要求入口存在、不要求端到端跑通，只要求没有把自己弄坏。

聚合失败 → 把失败输出和本波叶子清单打包成**一个修复叶子**插进树里，它就是下一波的第一件活。聚合连续失败 `-max-aggregate-fails` 次才放弃。

聚合通过 → 还有没做完的工作就回到 ①，没有就跑根目标验收（真正启动服务、执交付物），失败依旧是老规矩：路由给集成叶子重试，或者补一个。

```
wave=3, 假设目标是 6 个包的物理引擎：

cycle 1  拆 vec2            → 跑 vec2            → 聚合（还没有 .go 文件，跳过，见下）
cycle 2  拆 shapes, rigidbody → 跑两个            → 聚合 ✅ go build + go test
cycle 3  拆 collision, solver → 跑两个            → 聚合 ❌ collision 里 SAT 写错，rigidbody 还能编译
             └→ 插 repair 叶子（带着这条编译错误）→ 跑 repair → 聚合 ✅
cycle 4  拆 world, demo      → 跑两个            → 聚合 ✅
cycle 5  根目标验收 go test ./... && go run ./cmd/demo → ✅
```

第一圈没有源码时聚合是**跳过**而不是"通过"：刚建好的空 Go 模块里 `go build ./...` 退出 0、`go test ./...` 退出 1（`no packages to test`），照跑会把"工程还不存在"报成"工程坏了"，然后花一整波去修没人写的代码。显式 `-aggregate` 是操作者的契约，不受这条限制。

还有一个**基线**：跑任何叶子之前先量一次工作区。若工作区本来就有源码、而且本来就编译不过，这个失败指纹被记下来，之后复现同一指纹的波次不会排修复叶子——不是本次运行弄坏的，不该由它来修。空工作区不记基线：那时任何失败都归本次运行。

对比旧流程：同样 6 个包，旧流程是拆 13 个叶子 → 全跑 → `go run ./cmd/demo` 失败 → 重拆/补集成叶子 → 再全局装配一次。中间任何一步都可能已经坏了，但要浪费到最后才知道。

### 为什么这能解决"拼不回来"

| | 旧（开环） | 新（闭环） |
|---|---|---|
| 集成频率 | 最后一次 | 每一波结束 |
| 归因 | 全局失败，已无现场 | 紧跟那一波，工作区快照还在，能做文件重叠归因 |
| 拆解依据 | 目标文字 + 想象 | 目标文字 + 上一波的真实代码 + 上一轮聚合结果 |
| 计划错了怎么办 | 攒到最后重拆整棵树 | 聚合失败直接产出修复任务，只重拆出错的那一段 |
| 交付物状态 | 长时间处于"可能编不过" | 每个 cycle 边界都可编译可测试 |

一句话：**把"最后能不能拼回来"这个赌注，拆成每 3 个叶子一次的小赌注。** 每次都赢一点，攒起来就是大任务；某次输了，输的也只是那一小步。

### 边界：什么时候不该用这个循环

- **波太大等于没改**。`-wave` 设成 20 就是把开环问题原样搬回来：20 个叶子跑完才聚合一次，坏了同样归不了因。默认 3 是个折中（一次聚合的成本≈一次 `go test`）。反过来 `-wave` 小于 `-parallel` 时，并发池会被配额卡住，一波跑不满——两者保持 `-wave >= -parallel` 比较自然。
- **聚合检查不等于目标验收**。它只保证"没把自己弄坏"，保证不了"目标做完了"。后者仍然靠根目标验收 + `-dod`。
- **单模块小目标用 `-solo`**。一个上下文能装下的目标，聚合这一步的收益小于它带来的调度开销，别绕路。
- **聚合本身很慢时**（全量 `cargo test` 十几分钟），把 `-aggregate` 缩小成 `cargo build`，把测试留给根目标验收。

### 实施

六步都已落地，实现在 `pkg/agent/wave.go` + `orchestrator.go` 主循环：

1. **波节流**：`waveHasRoom` 按住拆解，直到"已规划未执行叶子数 < `-wave`"；`waveBudget` 同时按住**执行**，一波最多发起 `-wave` 个叶子（`TestRun_WaveBatchesExecution`：5 个叶子的平坦计划、`-wave 2` 必须聚合 3 次，去掉配额就只剩 2 次）。能跑的叶子为空时强制放行拆解（否则死锁），有拆解在途时一律不放行（拆解是异步的，`PendingLeafCount` 在它返回前还是旧边界，不拦就会超额放行一整棵子树）。
2. **聚合检查**：`runWaveAggregate` 复用 `Verifier` 的沙箱与超时，命令来自 `-aggregate` 或按技术栈推断。它**刻意不复用** `defaultDoDCommands`——那个是绑在叶子自己的产出上的（`test -f x.go`），整仓检查不能依赖某个叶子声明了哪些文件。
3. **主循环接线**：`waveBoundary`（配额跑满或计划榨干，且树未终结）触发聚合，且这个分支排在 `RootNeedsAcceptance` **之前**：验收是昂贵且不对人的端到端断言，工作区根本编不过时跑它只是浪费，而聚合失败有明确责任人。
4. **修复叶子**：`ensureRepairLeaf` 产出声明留空（它改的是已有文件）、依赖为空（必须立刻能跑），DoD 就是那两条聚合命令，同时把根置回 `RUNNING` 并清掉 `IntegrationVerified`。
5. **可观测性**：`wave_baseline` / `wave_aggregate` / `wave_repair` 事件；`-report` 出聚合次数、失败次数、修复叶子数，且只在循环真的跑过时才出现——`-wave 0` 的会话不会挂一排零。
6. **回归对照**：`-wave 0` 关掉节流、聚合、修复叶子，退回旧的平坦行为。单测 `TestRun_WaveThrottleDefersDecomposition` 同时跑 `throttled` 与 `flat` 两个子用例：把节流代码去掉，`throttled` 必须失败而 `flat` 必须照旧通过——它测的是行为，不是实现。



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

## 弱模型基准（divvy-bench）

引擎的立论是"用工程约束而不是模型聪明程度把复杂目标做完"，这个立论需要可重复的度量。`cmd/bench` 是一个固定的目标语料库 × 真实弱模型的基准 harness：

```bash
go run ./cmd/bench -list                                  # 看语料库
go run ./cmd/bench \
  -base-url http://127.0.0.1:11434/v1 -model qwen3.8:27b \
  -extra '{"reasoning_effort":"none"}' \
  -root bench-runs/$(date +%Y%m%d-%H%M%S)
```

每个目标在**全新临时工作区**里真实执行 divvy 全流程（拆解 → 叶子 → 真实 `go build`/`go test` 验收 → 根目标端到端验收），采集：成败、墙钟时间、LLM 调用数、token、成本、叶子数、以及引擎自身的失败计数器（验收失败/重试/停滞/重拆）。结果同时打印表格并写入 `<root>/report.json`，工作区保留供事后解剖（`git diff` 看模型到底改了什么）。

语料库刻意小且版本化在代码里（基准的目标集漂移就无法发现回归）；当前三个目标覆盖三种任务形态：从零创建包（`string-utils`）、修复 seeded bug（`fix-average`，语料自带失败测试，harness 测试会守住"这个目标真的会失败"）、构建可运行服务（`http-health`）。默认端点是本机 Ollama，方法论与参考数字见 **[docs/weak-model-bench.md](docs/weak-model-bench.md)**。

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
- **Prompt 与拆分粒度仍需按具体弱模型微调**，相关旋钮是 `-max-depth`、`-max-steps`、`-extra`（关掉 Qwen3 的 thinking 等）。
