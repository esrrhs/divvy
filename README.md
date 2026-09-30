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
        └─ 叶子 Worker：干净上下文 + 5 个工具
              list_dir / read_file / write_file / replace_lines / run_bash
              └─ Verifier 跑 DoD 命令（如 go test ./...）
                    ├─ 通过 → 向上冒泡 COMPLETED
                    └─ 失败 → 新的隔离上下文重试（带上错误，指数退避，上限 30s，默认无限次）
```

叶子执行**不携带**其它叶子的对话历史，只注入：当前任务、契约、父节点/依赖摘要、少量相关文件、验收命令。
没有依赖关系的就绪叶子可以并发执行（`-parallel N`，默认 `1`）；每次 LLM 调用的 token 用量按节点记入任务树并随会话持久化，运行结束打印本次与会话累计（resume 后自动累加），树状进度与 `-status` 里也会显示每个节点的消耗。

开启隔离（`-isolate`，`-parallel >1` 时自动生效）后，叶子在主工作区的**临时镜像副本**里写代码、跑验收命令：
验收通过才把新增/修改的文件合并回主工作区（删除也会传播），失败或中断则整个镜像丢弃——失败的尝试永远不会污染共享工作区或兄弟叶子。

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

弱模型上的 Prompt 与拆分粒度仍需按具体模型微调（`-max-depth`、`-max-steps`、`-extra`）。
