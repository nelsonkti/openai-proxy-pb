# OpenAI Proxy

一个只负责 API Key 配置和 OpenAI 兼容转发的 Go 服务：

- 支持 `/v1/chat/completions`（文生文，兼容流式 SSE）
- 支持 `/v1/images/generations`（图片生成）
- 支持 `/v1/models`
- 支持 `http://`、`https://`、`socks5://`、`socks5h://` 出站代理，并兼容常见拼写 `socket5h://`
- 每个客户端 Key 可独立绑定代理，并支持固定、轮询和故障切换策略
- 每个 Key 复用独立 HTTP Transport/连接池，带连接超时、响应头超时、请求超时、重试退避和 `Retry-After`
- 不包含账号登录、OAuth 或管理后台

## 启动

```bash
go mod tidy
go run ./cmd -config config.yaml
```

默认上游为 `https://api.openai.com`。将 `config.example.yaml` 复制为 `config.yaml`，按需修改 `project_key`、`server`、`logging` 和 `keys` 配置。`project_key` 是调用本代理服务时使用的项目 Key，不需要放在 `keys` 中。`logging.path` 填写日志目录，程序会自动生成 `openai-proxy.log`，同时输出到 stderr。

`logging.max_files` 表示日志总文件数，包含当前日志文件；单个日志文件默认超过 100MB 后自动压缩轮转。例如 `max_files: 7` 最多保留当前文件和 6 个历史文件。

图片生成等长耗时请求默认允许等待 30 分钟，包括服务端写响应、上游整体请求和等待上游首字节的时间。需要更长时间时，可在 `upstream` 中设置 `request_timeout`、`response_header_timeout`，并同步增大 `server.write_timeout`。

## 调用

```bash
curl http://127.0.0.1:8080/v1/chat/completions \\
  -H 'Authorization: Bearer client-demo' \\
  -H 'Content-Type: application/json' \\
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"你好"}]}'
```

图片接口：

```bash
curl http://127.0.0.1:8080/v1/images/generations \\
  -H 'Authorization: Bearer client-demo' \\
  -H 'Content-Type: application/json' \\
  -d '{"model":"gpt-image-1","prompt":"一只戴墨镜的猫"}'
```

如果未配置 `base_url`，默认使用 `https://api.openai.com`。`project_key` 是客户端调用代理时使用的 Key；`keys[].upstream_key` 是转发给 OpenAI 的 Key。如果省略 `upstream_key`，则默认使用该配置项的 Key（旧配置兼容）转发给上游。单个 `keys` 配置时可以省略 `keys[].key`，程序会自动使用 `project_key` 绑定该配置；客户端不需要额外传上游 Key。

## 代理策略

- `fixed`：默认策略。使用 `proxy`，或者 `proxies` 中的第一个代理。
- `round_robin`：每次请求按顺序轮换 `proxies` 中的代理。
- `failover`：按顺序尝试代理；网络错误以及配置的临时 HTTP 状态码会切换到下一个代理。
- `proxy` 是单代理兼容写法；配置多个代理时使用 `proxies`，二者不能同时配置。
- `proxy` 和 `proxies` 都未配置时默认直连，不设置任何出站代理。
- 为避免普通 POST 被重复提交，`failover` 只会对 GET/HEAD/OPTIONS、配置了 `Idempotency-Key` 的请求，或显式允许非幂等重试的请求切换代理；流式请求不跨代理重试。
- 单条 `keys` 配置时可以省略 `keys[].key`，直接使用顶层 `project_key`；配置多条线路时，需为每条线路填写不同的 `keys[].key`，请求使用对应 Key 选择线路。

示例：

```yaml
keys:
  - upstream_key: sk-your-openai-key
    strategy: round_robin
    proxies:
      - http://127.0.0.1:7890
      - socks5h://127.0.0.1:1080

  - upstream_key: sk-your-openai-key
    # 未设置 proxy/proxies，默认直连
```

## 稳定网络策略

- 对连接失败、超时和配置的临时 HTTP 状态码重试；请求体会在内存中缓存以支持重试。
- 默认不重试非幂等请求，避免普通 POST 在上游已处理但响应丢失时重复扣费。
- 普通 POST 默认不重试；如果连接在请求头写出前失败，会安全重试有限次数；请求带有 `Idempotency-Key` 时还允许按状态码有限重试，流式请求不会自动重试。
- 429/503 等响应优先读取 `Retry-After`，否则使用指数退避并加入抖动。
- SSE 流式响应按块转发，不缓冲完整响应。
