# Web Search 与 Web Fetch 上游还原方案

> 历史设计与调研记录。当前行为、配置和命令以[文档索引](README.md)中的指南为准。

对齐基线为官方 DeepSeek Harness `fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159`。使用该提交的共享产品配置和 Web 的 Cordis、PTC、Standard 预设。共享配置选 DeepSeek Search API 和本地 HTTP Fetch，搜索超时 60 秒，抓取超时 30 秒。工具包本身的搜索默认超时是 30 秒，产品配置覆盖为 60 秒。Go 仓库的 v4 会话迁移基线仍由现有兼容性文档记录。 [共享产品配置](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/bundle/base/cordis.patch.yml) · [工具包默认值](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/web/tool-web/src/index.ts)

## 实施结果

- `web_search` 使用供应商无关的强类型结果，处理单条保序、多条并发归并、摘要限长及 v4 来源元数据。`deepseek` adapter 发送本轮最近一条用户消息的文本摘要 `ori_question`，按一秒窗口限流。部署密钥仍来自 `WEB_SEARCH_API_KEY`，因此配置错误的修复提示采用本仓库的配置字段。
- `web_fetch` 的 HTTP provider 只负责请求、解码和限制正文。HTML 格式化在 Go Host 的 Goja 中执行固定上游渲染代码，嵌入 Turndown、GFM 和 Domino 的锁定版本。运行时无需 Node 进程。Go 按 UTF-8 保存模型文本，在 UTF-16 截断恰好切断代理对时使用 U+FFFD。
- 两个工具的模型文本与 v4 元数据分别输出。超出共享配置的 12,500 token 行内预算时保留头尾，并把完整格式化结果写入会话工作目录的 `.agent/web-spill/`。云 Host 保留公开地址校验、DNS 拨号绑定和匿名抓取。
- 历史记录按原工具名回放。公开 SSE 与事件 API 保留 v4 `tool/result.data.meta`。冷会话通过发新消息恢复运行环境后可以继续读取事件流。

下文的差异表记录还原前的 Go 行为和基线，作为迁移审计。 [当前验收记录](validation.md)

## 已落地的配置和供应商装配

- 启动脚本在目标 `.env` 缺失时用模板生成私有文件，两个搜索字段初始留空。合并环境覆盖后，只有生效的模型地址是官方 `https://api.deepseek.com` 时，脚本才自动填充 `deepseek`，并用本次显式传入的 `MODEL_API_KEY` 初始化空的搜索密钥。自定义模型地址须显式提供两项搜索配置。显式空值会校验失败，已有搜索密钥在模型轮换时保留。
- `controller serve` 进程启动时读取保存的配置，并在打开状态目录、初始化 driver 或监听 HTTP 前校验供应商和搜索密钥。Host 独立启动时执行同样的必填与注册检查。
- Host 按配置中的名字在 `providers/web/registry.go` 查找 factory。`deepseek` 使用独立的 Search API `POST https://api.deepseek.com/v1/tools/search`。新增供应商时实现 `web.SearchProvider` 并登记 factory，使用同一套工具。找不到登记项时启动失败。抓取仍走 Host 本地 HTTP。 [配置说明](configuration.md) · [供应商扩展](providers.md)

## 搜索工具的差异

| 环节 | Go Host 当前实现 | 上游目标 |
| --- | --- | --- |
| 工具名及参数 | `websearch`，单次 1–4 条，每条最多 100 个 Unicode 码点 | `web_search`，必填 `queries`，1–10 条非空白字符串，先校验数量再按原字符串去重。工具层无单条 100 字限制 |
| 调用与归并 | 每条经兼容 JSON 包装，一律经过归并。并发、首错取消并等兄弟任务退出、按排名轮询、按 URL 去重、总上限 10 已有 | 单条直接用 web service 的受限结果，保留供应商顺序与重复项。多条保留并发、首错收敛和轮询归并。总上限 10，供应商结果先按每条请求的 `maxResults` 截断并标记 |
| 标准结果 | 工具最终只返回字符串。旧包装使用 `success/data/web/structured`，把来源 `summary` 拼成独立正文 | 标准值为 `{ content?, sources, truncated }`。来源为 `{ url, title?, snippet?, summary?, publishedAt? }`。仅供应商真正给出的综合回答使用 `content`，多查询才按查询标题合并回答 |
| 模型可见文本 | 有不可信提示、Markdown 来源列表、无结果提示、截断提示和引用提示。来源行取 `Description`，摘要可能在正文再次出现 | 保持上游各段顺序、空行和标题降级规则。每条来源优先展示压缩空白后的 `summary`，为空时用 `snippet`，可附发布日期。摘要上限 800 UTF-16 单元，按上游规则加省略号。直接 Search API 通常只有来源 |
| 回放与错误 | 记录文本，执行错误多为通用 `TOOL_ERROR` | 另存 `sources`、`truncated` 和可选 `answer` 的 presentation meta，来源卡片保留 `snippet` 而无需复制摘要。按上游层级对齐参数错误、工具超时和供应商错误 |

当前 DeepSeek 适配器已经调用同一 Search API，差异还包括 `ori_question`、限流和错误映射。上游在有触发搜索的用户原问题时压缩空白并限制到 128 个码点，才发送 `ori_question`。上游默认在一秒窗口最多启动 5 个请求，当前以 200 毫秒间隔发起。上游对响应字段和日期类型做完整校验，当前只校验结果数组及非空 URL 等部分约束。实现时将这些行为放在 DeepSeek 适配器或其 API 封装里。Go 的显式供应商选择和部署密钥来自 `.env`。 [上游搜索工具](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/web/tool-web/src/search.ts) · [DeepSeek 搜索适配器](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/web/web-deepseek-api/src/search.ts) · [上游请求与错误处理](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/web/web-deepseek-api/src/transport.ts)

## 抓取工具的差异

| 环节 | Go Host 当前实现 | 上游目标 |
| --- | --- | --- |
| 名称与校验 | 对模型叫 `webextract`，工具层同时校验空值和 2,048 字符长度 | `web_fetch`，工具层校验 `url` 非空白，HTTP provider 校验长度、协议和 URL 凭证 |
| HTTP 行为 | 匿名 GET、最多 5 跳同源重定向，30 秒总预算。内容类型匹配大体相同。Go User-Agent、严格 MIME 参数解析、额外连接子超时 | 同样的跳数与总预算。对齐上游 User-Agent、URL 正规化、MIME 分类、编码标签和 UTF-8 回退。声明长度超过 5,000,000 字节时报错，未知长度读到上限时截断。解码正文上限 100,000 UTF-16 单元。支持的非 2xx 正文也保留 HTTP 状态 |
| Provider 返回值 | Provider 内完成 Markdown 转换，返回 `URL/StatusCode/Text/Truncated` | `{ url, statusCode, body: { kind: "html" | "text", content }, truncated }`。Provider 只做取回和解码，转换发生在工具输出层 |
| HTML 转换 | 自写渲染器保留部分 Markdown 结构并过滤隐藏节点，处理链接和空白的规则与 Turndown 有差异。超深 HTML 返回省略标记并设截断 | 对齐 Turndown 和 GFM 产生的 ATX 标题、列表、链接、围栏代码、表格及删除线。对齐活动/隐藏节点过滤、表格不展开 span、深度 512 与转换失败省略标记。省略本身与正文被截断分别计算 |
| 模型文本与回放 | 已有最终 URL、HTTP 状态、不可信内容提示和截断后缀，整体上限 200,000 UTF-16 单元。缺结构化抓取 meta | 先限制转换输入，再计算包含头部及后缀的 200,000 UTF-16 单元上限。meta 记录最终 `url`、`statusCode` 和有效 `truncated`，与正文的截断状态一致。对齐上游成功与错误展示 |

云 Host 保留 public-only 网络边界，拨号绑定已检查的公开地址，同源跳转也逐跳验证。上游本地 HTTP provider 可以访问私有地址并使用进程代理策略。Go 在 Docker 和 ECI 中保持禁止访问私网及云元数据的策略，正文、公开站点抓取及返回契约按上游对齐。 [上游抓取工具](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/web/tool-web/src/fetch.ts) · [上游 HTTP provider](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/web/web-fetch-http/src/provider.ts) · [上游网络边界](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/docs/subsystems/web.md#fetch-network-policy)

## 实施顺序

1. **先固定上游输出样本。** 从该提交运行搜索和抓取测试，以固定的搜索响应和 HTTP 页面生成 JSON 标准值、模型文本、错误和 presentation meta 四类样本。将输入和预期输出放入 Go 测试目录。测试运行时使用本地 fixture，生产构建继续是 Go。
2. **拆分 provider 与工具格式。** `web.SearchProvider` 返回上游形状的强类型结果，删除内部的 `success/data/structured` 往返。抓取 provider 返回原始解码正文和 `kind`，移出原 HTML 渲染调用。由 `harness/tools` 负责上游的参数、归并和渲染。固定源代码的参考样本校验空白、链接、表格、转义、嵌套和 Unicode 截断。
3. **贯通模型、会话与前端。** 在工具声明、执行和系统提示中使用 `web_search` 与 `web_fetch`。如需迁移 PTC 绑定，从同一标准结果读取结构化值。给 native tool result 增加可选 meta 和类型化错误，把它们按上游 v4 事件形状写入并通过 SSE/回放提供给前端。历史事件按原名称和文本读取，工具名切换只影响新执行。复用原有通用结果限制，检查它在正常上游样本下不会改写工具输出。
4. **验证 Host 全链路。** 用假的 Responses 模型同时验证工具声明、实际调用、下一轮模型收到的精确文本、SSE 和磁盘事件，再用固定上游提交的严格解码器验证新 journal。另用旧 journal 验证恢复时不会重跑已完成的旧工具。

## 验收用例

- 搜索覆盖 1/10/11 条查询、重复与空白查询、单条保序、多条轮询去重、超量、空结果、摘要与片段择一、Unicode 截断、首错取消并等待、HTTP 401/429 与坏响应。切换到测试 factory 后，同一份结果的标准值、模型文本和 meta 应完全相同。
- 抓取覆盖同源重定向、缺失 Location、非 2xx、声明与流式字节超限、字符集和 BOM、未知 MIME、纯文本、带隐藏元素的 HTML、Markdown 转换和过深 DOM。另测私有地址拒绝和跳转后的网络边界。
- 按字节比较固定输入的模型文本，按 JSON 结构比较标准值、错误和 meta。日志、SSE 与冷恢复分别验证。
