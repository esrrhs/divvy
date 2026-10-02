# 本机 Ollama 运行 Qwen3.8-27B 并接入 divvy

本文记录在 macOS 上用 Ollama 运行 `qwen3.8:27b` 的准确标识、拉取踩坑，以及引擎侧的适配参数（思考控制、原生工具、验收语义）。

## 模型信息（以官网为准，勿凭记忆猜 tag）

| 项 | 值 |
|---|---|
| 准确标识 | `qwen3.8:27b`（`qwen3.8:latest` 同为 27B） |
| 体积 | 约 18 GB（Q4，dense 27.8B） |
| 上下文 | 256K |
| 输入 | 文本 + 图像（多模态，引擎只发文本不受影响） |
| 思考 | 默认开启，可按请求关闭；支持 `reasoning_effort` 调节深度 |
| 工具调用 | 原生支持（`tool_calls`），同时严格遵循引擎的 JSON 动作格式 |

## 拉取

```bash
ollama pull qwen3.8:27b
```

实测 registry 连接会反复在中途 `unexpected EOF`。已下载的 blob 有缓存，用重试循环自动断点续传即可：

```bash
until ollama pull qwen3.8:27b; do echo "resuming..."; sleep 3; done
```

## 思考控制（与 Gemma/Qwen3 老参数不同）

- 思考内容通过消息的 `reasoning` 字段返回，引擎只读 `content`，天然不受影响。
- **Ollama 0.34.4 对该模型忽略 `enable_thinking:false`**（传了不报错但思考照写）。
- 真正生效的是 `reasoning_effort`：实测同一编码任务 completion 385 → 117 token（约 3 倍），`"none"` 时 `reasoning` 为空；`"low"` 轻度缩减。

```bash
-extra '{"reasoning_effort":"none"}'   # 最快、最省；叶子执行推荐
-extra '{"reasoning_effort":"low"}'    # 折中
# 不传 = 默认思考；复杂目标的拆解阶段保留思考，质量更好
```

## 跑通引擎

```bash
go build -o divvy ./cmd/divvy
mkdir -p ws && cd ws && git init -q && git config user.email you@x.com && git config user.name You && go mod init demo && cd ..

./divvy \
  -base-url http://127.0.0.1:11434/v1 \
  -model qwen3.8:27b \
  -workdir ./ws -git-commit -isolate \
  -max-retries 2 -max-steps 15 -timeout 10m \
  -extra '{"reasoning_effort":"none"}' \
  "用 Go 写一个 HTTP 服务：监听 :18080，GET /health 返回 200 和字符串 ok；并写单元测试"
```

注意必须显式传 `-base-url` 和 `-model`（否则默认指向 OpenAI 官方端点并要求 API key）。

实测「/health HTTP 服务 + 单测」任务：拆解判定原子 → 5 步工具调用 → `go build`/`go test` 验收通过 → 每叶子一个提交，共 6 次 LLM 调用、5808 token、约 3.5 分钟。生成代码自带 200/405 两条测试，服务实测 `GET /health → 200 ok`、`POST → 405`。

## 模式选择

- **JSON 动作模式（默认）**：该模型严格输出 `{"thought":...,"action":...,"args":{...}}`，稳定可用，仍是推荐默认。
- **`-native-tools`**：实测 `/v1` 下能产出规范的 `tool_calls`（参数 JSON 正确），27B 强模型可开，减少格式纠偏回合。

## 验收语义

接入实测中发现：多命令 DoD 带 `expected_output` 时，旧逻辑要求每条命令输出都包含该文本，安静的 `go build` 会让验收永远失败、叶子被无谓重拆。现已改为：所有命令退出码为 0 后，对**全部命令的合并输出**做一次 `expected_output` 匹配（见 `pkg/agent/verifier.go`）。
