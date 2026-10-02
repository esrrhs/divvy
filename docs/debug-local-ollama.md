# 用本地 Ollama 调试 divvy

本文记录在本机用 Ollama + Gemma 调试引擎的完整配置，以及本地弱模型特有的调参项。

## 环境准备（macOS）

```bash
brew install ollama
brew services start ollama          # 常驻服务，端口 11434
ollama pull gemma4:12b              # 7.6GB；32GB 内存可舒适运行
```

验证 OpenAI 兼容端点：

```bash
curl -s http://127.0.0.1:11434/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma4:12b","messages":[{"role":"user","content":"Say OK"}],"max_tokens":100,"stream":false}'
```

## 运行引擎

```bash
go build -o divvy ./cmd/divvy

export OPENAI_BASE_URL=http://127.0.0.1:11434/v1
export OPENAI_MODEL=gemma4:12b
# 本机地址无需 API key

./divvy -workdir ./ws "创建文件 hello.txt，内容为：hello"
```

全链路（隔离执行 + 每叶子一个 git 提交）：

```bash
mkdir -p ws && cd ws && git init -q && git config user.email you@x.com && git config user.name You && go mod init demo && cd ..
./divvy -workdir ./ws -git-commit -max-steps 12 -timeout 10m \
  "为现有模块 demo 添加 Add(a, b int) int，写在 add.go，并在 add_test.go 带单元测试"
git -C ws log --oneline   # 查看每叶子的审计提交
```

## 本地模型必调的参数

| 参数 | 默认 | 为什么 |
|---|---|---|
| `-timeout` | `120s` | **最容易踩的坑**。本机 12B 模型实测 ~6-15 tok/s，带思考的单次调用可达数千 token，经常超过 120s。本地调试建议 `-timeout 10m`。 |
| `-max-steps` | `20` | 单叶子最多工具调用轮数。慢机器上可调小（如 12）控制单叶子耗时。 |
| `-max-retries` | `0`（无限） | 调试时可设 `2` 避免一个坏叶子无限烧时间。 |

## Gemma 4 上的已知行为

- **思考不可关**：Ollama 0.34.x 的 `/v1` 端点对 gemma4 忽略 `think:false`，模型总会在 `reasoning` 字段输出思考内容。引擎只读 `content`，不受影响，但响应更慢、`max_tokens` 要给足（默认 4096 够用）。
- **流式 usage**：OpenAI 兼容协议下流式请求需带 `stream_options: {"include_usage": true}` 才能在最后一个 chunk 拿到 token 用量——引擎已自动发送，树视图与用量汇总在 Ollama 下可正常显示。
- 一次解构调用若把思考写满 `max_tokens`，`content` 可能为空，引擎会把"无法解析工具调用"回喂给模型重试，这是预期行为。

## 网络备注

`registry.ollama.ai` 在部分网络下会限速到 KB/s 级（Cloudflare 路径问题）。遇到下载龟速时，重启服务换连接通常有效：

```bash
brew services restart ollama && ollama pull gemma4:12b   # 断点续传
```

## 调试技巧

- `-v` 打印模型原文与工具输出，配合 `-no-stream` 更容易看完整响应。
- `-plan -strict` 先检查拆解质量再执行；弱模型拆解产出的告警大多值得认真对待。
- 会话保存在工作区的 `.divvy/`，`-status` 看任务树，`-resume` 续跑（含 Ctrl+C 中断后）。
- `git -C ws log` 配合 `-git-commit` 可精确定位每个叶子改了什么。
