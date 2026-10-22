# WorkBuddy2API Panel

WorkBuddy2API Panel 是一个自托管的 OpenAI 兼容反向代理网关，将腾讯 CodeBuddy 账号接入统一的 API 服务，并提供内嵌 Web 管理面板。

当前仓库按**私有 / 闭源方式维护**，不接受公开需求、贡献或对外分发。

> [!IMPORTANT]
> 后续持续同步上游 [misakano7545/workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel/) 的 `main` 变更。合并时必须保留本仓已有的分发密钥、登录失败锁定、账号绑定、协议兼容和运维增强能力。

> [!WARNING]
> 本项目是非官方网关，仅供账号持有人在本机或私有环境使用。不得搭建对外中转站、售卖额度、共享凭据或用于违反上游服务条款的用途。账号风控、封禁和条款风险由使用者自行承担。

## 核心能力

- 入站兼容 `chat_completions`、`codex_responses` 和 `anthropic_messages`
- 多账号 OAuth 登录、静态 API Key 账号、账号池轮转与故障切换
- 账号级冷却、模型级限流、熔断、在途租约和会话粘性
- 流式与非流式响应，思考内容、工具调用和用量统计转换
- 原生工具调用标记还原、Codex Responses 工具调用错误显式化
- 内嵌 Web 面板：账号、密钥、模型、用量、配置、任务和运行日志
- 分发密钥与管理密钥隔离，支持全局或绑定指定账号
- 登录失败锁定、结构化脱敏日志、原子持久化和密钥落盘回滚
- 定时签到、活跃上报、旅行、保活、余额刷新和成长任务队列
- 可选 Upstash Redis 状态镜像、Prometheus 指标、管理端任务和告警 webhook

## 架构

```text
OpenAI / Codex / Claude 客户端
              |
              v
       鉴权与协议适配
              |
              v
账号池、粘性、冷却与预算控制
              |
              v
    CodeBuddy Chat Completions SSE
```

`/v1/responses` 与 `/v1/messages` 会先转换为 Chat Completions 请求，再进入统一的上游请求管线。非流式请求由网关聚合上游 SSE 后返回单次 JSON 响应。

## 快速开始

### Docker Compose

```bash
git clone http://192.168.100.235/Web/workbuddy2api.git
cd workbuddy2api
cp config.example.json config.json
docker compose up -d --build
curl -s http://127.0.0.1:7863/healthz
```

打开 `http://127.0.0.1:7863/panel/`，通过面板添加账号。

常用操作：

```bash
docker compose logs -f
docker compose restart
docker compose down
```

账号凭证与运行数据分别保存在 `auths/` 和 `data/`。停止或重建容器不会删除这两个目录。

### 本地二进制

```powershell
go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server
.\wb2api.exe -config config.json
```

前端资源已嵌入二进制。运行时只需要保证 `auths/`、`data/` 和配置文件所在目录可写。

### 源码开发

```bash
go build ./...
go vet ./...
go test ./...
go run ./cmd/server -config config.json
```

## 添加账号

### Web 面板

进入 `http://127.0.0.1:7863/panel/`，点击「添加账号」并完成 OAuth 设备授权。凭证会落盘并热加载到账号池，无需重启。

### 命令行

Linux / macOS 可使用：

```bash
./login.sh
```

### 静态 API Key

在面板「添加账号」中切换到 API Key 模式，填写平台签发的 `ck_...` 或 `pt_...` Key。静态 Key 账号参与对话、余额查询和选号，但不参与个人账号的签到、成长任务和连登活动。

## API

| 端点 | 说明 |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions，兼容流式与非流式 |
| `POST /v1/responses` | Codex Responses API |
| `POST /v1/messages`、`POST /messages` | Anthropic Messages API |
| `GET /v1/models` | 模型目录及档位、倍率、上下文等能力信息 |
| `POST /v1/search` | 上游联网检索；请求可带 `"deep": true` |
| `GET /status` | 账号池、冷却、模型锁与粘性状态 |
| `GET /healthz` | 无鉴权健康检查 |
| `GET /metrics` | Prometheus 指标，需启用 `metrics.enabled` |

`api_key` 非空时，除 `/healthz` 外的数据面和管理端请求需要 Bearer 鉴权。`api_key` 为空时网关直接放行，只适用于隔离的私有环境。

### Responses 与 Codex

- `GET /v1/responses` 固定返回 `426 Upgrade Required`，让 Codex 立刻回落 HTTPS POST。
- 网关不保存 `previous_response_id` 或 `store`。
- `finish_reason=length` 映射为 `response.incomplete`，不会伪装成正常完成。
- 上游工具调用全部缺少函数名时，流式返回 `response.failed`，错误码为 `upstream_tool_call_dropped`；非流式返回 HTTP 422。
- 只要本回合仍有合法工具调用，正常完成语义不变。
- 上游省略工具 `index` 时，只有新的非空 `call_id` 才开启新调用；普通续帧归入最近调用。

## Web 面板

面板路径：

```text
http://127.0.0.1:7863/panel/
```

主要视图：

- 账号池：健康状态、积分、冷却、在途、粘性和单账号操作
- 密钥：签发、启停、重置、删除和账号绑定
- 用量：请求、Token、延迟、积分与时间区间统计
- 模型与档位：模型能力、思考档、倍率和繁忙度
- 配置：常用配置热更新
- 运行日志：请求记录、服务日志和积分历史
- 任务中心：成长任务扫描、执行、补领和排程

## 配置

[config.example.json](config.example.json) 是完整配置参考。最常用的字段如下：

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7863` | HTTP 监听地址 |
| `api_key` | 空 | 根密钥；空表示不鉴权 |
| `auth_dir` | `./auths` | OAuth / API Key 账号凭证目录 |
| `state_file` | `./data/state.json` | 账号池状态文件 |
| `keys_file` | 空 | 分发密钥文件；空时使用 `state_file` 同目录的 `keys.json` |
| `security.login_lockout` | 开启 | 按来源 IP 的鉴权失败锁定 |
| `prompt.mode` | `custom` | `custom` / `append` / `passthrough` |
| `models.hidden` | `[]` | 对外隐藏并拒绝点名的模型 |
| `models.aliases` | `[]` | 客户端模型名到真实模型名的映射 |
| `pool.max_in_flight` | `3` | 单账号最大在途请求 |
| `pool.reserve_credits` | `10` | 账号最低保留积分 |
| `session_sticky.enabled` | `true` | 会话粘性 |
| `schedule.*_enabled` | `true` | 各类定时任务开关 |
| `upstream.timeout_seconds` | `120` | 短 RPC 总超时 |
| `upstream.idle_timeout_seconds` | `300` | 聊天流空闲超时 |

环境变量可覆盖部分 JSON 配置。支持项以 `config.example.json` 和 `cmd/server/config.go` 为准。

修改面板可热更新的字段无需重启；`listen`、`auth_dir`、`state_file`、`upstream.*`、`upstash.*` 等装配期字段保存后会提示重启。

配置示例见 [config.example.json](config.example.json)。重启项以面板保存后的提示为准。

## 定时任务

任务均使用 `Asia/Shanghai` 自然日和时钟口径，不受宿主机时区影响。空数组或 `null` 表示回落默认时点，不代表关闭；使用 `schedule.*_enabled: false` 才能真正停用。

| 任务 | 默认时点 | 说明 |
|---|---|---|
| 签到 | `[9, 21]` | 签到、余额查询并解冻恢复余额的账号 |
| 旅行 | `[9, 21]` | 猫猫旅行领奖、派出和领养 |
| 活跃上报 | `[10]` | 点亮个人账号活跃与连登状态 |
| 保活 | `[22]` | 刷新账号 token |
| 夜猫子 | `[23]` | 在任务窗口内补足夜间对话 |
| 成长队列 | `[11]` | 扫描并执行任务中心待办 |

其余周期任务：

- `schedule.balance_refresh_minutes`：余额刷新，默认 5 分钟
- `schedule.task_sweep_minutes`：任务补领清扫，默认 15 分钟

## 本仓维护能力

同步上游后必须继续保证以下功能：

- 分发密钥持久化、启停、过期和重置
- 分发密钥只能访问数据面 `/v1/*`
- bound 密钥只能使用绑定账号，失败时不得借用其他账号
- 粘性会话同样遵守账号绑定范围
- 面板与 `/v1/*` 共用登录失败锁定计数
- 密钥写入失败时回滚内存状态并保留可诊断日志
- Responses 工具调用、Anthropic Messages 和 Chat 协议转换回归测试
- Codex 工具调用静默完成防护

## 同步上游

固定上游：

```text
https://github.com/misakano7545/workbuddy2api-panel/
```

当前仓库配置：

```bash
git remote -v
git fetch upstream --prune
git switch main
git merge --no-ff upstream/main
```

同步原则：

1. 新功能整体吸收，不凭提交标题猜测行为。
2. 与本地密钥、鉴权、绑定路由、面板和协议修复冲突时，按最终语义合并。
3. 合并后运行 `gofmt`、`go vet ./...`、`go test ./...` 和 `go build ./...`。
4. 发布前检查 `git diff --check`，确认工作区干净。
5. 保留合并前备份分支，至少保证上游最新 `main` 已全部进入本地历史。

## 日志与排障

- 聊天请求会输出 TTFB、输出 Token、速率和实际账号等结构化日志。
- 请求归档默认只记录元数据，不写提示词、响应正文或凭证。
- 工具调用丢弃日志只记录路径、`call_id` 是否存在、参数长度和 `finish_reason`，不记录参数内容。
- `/healthz` 返回服务身份字段；需要强健康检查时使用带鉴权的 `/status`。
- 完整诊断可启用 `WB2A_DUMP_UPSTREAM`，仅在受控环境中使用，原始报文可能包含敏感内容。

## 安全

- `auths/`、`data/`、`config.json` 和分发密钥文件不得提交到仓库。
- 默认仅建议监听 `127.0.0.1` 或私有网络。
- 对外提供服务时必须设置强 `api_key`，并置于 HTTPS 反向代理与访问限流之后。
- 分发密钥明文仅在创建或重置时显示一次。
- 面板和表单输入按不可信内容处理，不把账号备注、模型名或上游文本拼入未转义 HTML。
- 不提交真实账号截图、上游原始报文、请求正文或包含个人信息的日志。

## 常见问题

### 模型繁忙和账号限流如何区分？

`6004` 与 `14003` 是模型级信号，只停用对应的「账号 + 模型」组合，换模型仍可使用。普通 429 才进入账号级软冷却与指数退避。

### Codex 答一句就停怎么办？

先确认网关版本包含 `upstream_tool_call_dropped` 处理，并查看日志中是否出现无函数名工具调用。该形态现在会返回明确失败，不再以空工具回合伪装成功。

### 登录后无法写入 `auths/` 怎么办？

检查挂载目录属主和容器运行用户。Docker Compose 可通过 `PUID`、`PGID` 使用宿主机用户运行，或把 `auths/`、`data/` 的属主调整为容器用户。

### 如何恢复已禁用账号？

优先重新登录并覆盖凭证。因 session 失效被禁用的账号，重新登录成功后会自动回池。

## 许可证

仓库中原有的 `LICENSE` 和历史版权声明继续有效。“私有 / 闭源维护”描述当前维护与分发方式，不追溯改变已发布版本的许可证。
