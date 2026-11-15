# Go Harness Web 聊天预览版方案

> 历史设计与调研记录。当前行为、配置和命令以[文档索引](README.md)中的指南为准。

实现已随 Controller 本地启动入口接入。 [验收记录](validation.md#web-chat-preview)

## 产品范围

使用 **React + TypeScript + Vite** 实现浏览器聊天页。用户按现有方式启动 Controller 后，打开 `http://127.0.0.1:8080/` 即可使用。首版覆盖新建会话、从侧栏打开已有会话、发送文本和接收流式回复，以及在会话列表行上右键删除。新会话使用 Host 的现有模型配置。

首页和空会话共用欢迎布局。打开已有消息的会话时，同一主栏显示聊天记录，底部保留同款输入框。浏览器可直接打开或刷新 `/chat/<session-id>`。首版的导航只放 **New chat** 和 **Chats**，输入区只放文本输入和发送按钮。产品名使用 **Go Harness**。

## 视觉与交互

以用户提供的 Aetheris 截图为参考。页面沿用原图的浅蓝与淡紫渐变、半透明左栏、白色圆角按钮、冷色玻璃质感和居中机器人图标。截图的透视角度属于展示效果，浏览器页面保持水平。还原时以截图中的空间比例、文字层级、颜色过渡和留白为准。

| 区域 | 设计 |
|---|---|
| 页面框架 | 全屏淡蓝到淡紫渐变，左下保留一抹更饱和的蓝紫。左栏在 1440 px 宽度下约 280–300 px，主面板左上大圆角。 |
| 左栏 | 顶部放小渐变标记和 Go Harness 字标。下方用醒目的浅色圆角 **New chat** 按钮对应参考图，后接可滚动的 **Chats** 列表。选中项使用浅色底，其他行随悬停轻微变亮。 |
| 欢迎区 | 顶部居中显示 `Go Harness Chat`。主区域居中放约 120 px 的浅蓝紫图标底座和 Lucide Bot 机器人图标、大字号时段问候和一行 `How can I help you today?`。问候按浏览器本地时间生成，首版没有姓名设置。 |
| 输入区 | 大圆角半透明白色面板，宽度随主栏收缩，桌面上限约 900 px。输入区保留截图中的宽阔留白，发送按钮在右下角。空页放在欢迎区下方，有历史时固定在聊天列底部。 |
| 聊天记录 | 和输入框同宽。用户消息使用浅蓝紫圆角气泡，助手回复使用清晰的正文排版。展示 Markdown 段落、列表和代码块。等待回复、断线或失败时，在对应消息附近给出状态。 |
| 颜色与字体 | 初始样式参考 `#E3EFFA` 浅蓝、`#E8E5F8` 淡紫、`#CDD5F8` 侧栏和 `#20272C` 正文。使用 Inter 和系统字体回退。机器人图标使用本地构建的 Lucide 组件，底座采用柔和阴影。 [Lucide Bot](https://lucide.dev/icons/bot) |
| 窄屏 | 主栏单列布局，左栏由顶部按钮打开为抽屉。输入框随可视高度及软键盘调整，聊天列独立滚动。支持减少动态效果的系统偏好。 |

打开根路径时显示欢迎页，用户可点 **New chat** 或直接在欢迎页输入并发送。点 **New chat** 立即调用创建接口，成功后选中新会话并聚焦输入框。根路径直接发送时先创建会话，再提交这条消息。页面刷新本身不创建会话。Enter 发送，Shift+Enter 换行，输入法合成期间不触发发送。

在列表行上右键显示一个 **Delete chat** 菜单项。键盘菜单键或 Shift+F10 打开同一菜单，触屏用行尾操作按钮。确认框显示会话名，并说明删除会话后会从列表消失。服务端确认后移除该行，若删除的是当前会话则回到欢迎页。失败时保留该行和当前选中状态。Esc 关闭菜单或确认框，焦点回到列表行。

## Controller 托管与目录

```text
浏览器
  ├─ 页面和静态资源 → Controller
  └─ /v1/...、SSE → Controller → Host（Docker 或 ECI）
```

Controller 在现有回环地址和端口上直接提供 Web 构建产物。浏览器通过相对 URL 调用现有 API，同一地址适用于 Docker 和 ECI。Controller 继续发现 Host 地址、检查就绪和注入内部 Host Token。Host 负责执行会话请求与读写会话数据。 [Controller 代理](../internal/controller/controller.go#L421) · [现有 HTTP API](api.md)

- `GET /` 和有效的 `GET /chat/<uuid>` 返回页面入口。带内容哈希的 `/assets/*` 资源长期缓存，入口 HTML 每次重新验证。缺失资源、未知路由和未知 API 返回各自的 404。健康检查和 Host 管理接口维持原有路由。
- 开发时 Vite 监听回环地址并代理 `/v1` 到本地 Controller，React 可独立热更新。发布时用 `npm ci` 和 Vite 生成静态产物，由带 `webui` 构建标签的 Controller 二进制嵌入。Go 嵌入声明放在产物同级的 `frontend/`，基础 Go 构建走无 Web 资源的实现。 [Vite 生产构建](https://vite.dev/guide/build) · [Vite 开发代理](https://vite.dev/config/server-options#server-proxy) · [Go embed](https://pkg.go.dev/embed)
- 默认 `scripts/start.sh` 通过 Compose 构建 Controller 镜像。也可以用本地 Node 构建前端，或用 Controller Dockerfile 的 `controller-binary` 阶段导出带前端资源的二进制。前端产物随 Controller 发布，构建上下文排除宿主机的 `frontend/dist` 和 `node_modules`。 [启动脚本](../scripts/start.sh) · [Controller Dockerfile](../build/Dockerfile.controller)
- 浏览器按同源部署。Web 版 Controller 将浏览器 API 请求限制为同源，Vite 开发联调只通过开发参数显式允许指定回环源，原生 CLI 无 Origin 的请求维持原契约。Host Token 和模型凭证留在后端。聊天 Markdown 使用安全渲染，禁用原始 HTML 和脚本协议链接。 [现有 Controller 请求检查](../internal/controller/controller.go#L460)

建议目录如下。

```text
frontend/
  package.json
  package-lock.json
  vite.config.ts
  index.html
  embed_release.go
  embed_disabled.go
  src/
    App.tsx
    api/             Controller HTTP 与 SSE 连接
    chat/            聊天记录、输入、事件状态更新
    sessions/        会话列表、新建、上下文菜单
    styles/          颜色、间距、响应式样式
  tests/             前端状态与浏览器验收
  dist/              构建产物，Git 忽略
```

仓库现有的 `web/` 是 Go 的网页工具包，Web 客户端独立放在 `frontend/`。首版使用 React 自带状态管理和普通 CSS。会话事实由 Host 保存，浏览器 URL 保存选中的会话，输入草稿和流式片段留在页面内存中。

## API 与数据行为

| 页面操作 | API | 行为 |
|---|---|---|
| 会话列表 | `GET /v1/sessions?limit=&cursor=` | 沿用服务端 `nextCursor`，支持继续加载并按会话 ID 去重。现有游标按 UUID 排序，界面可对已加载的行按 `createdAt` 排列。首版支持可选 `title`，由第一条已持久化的人工用户消息生成短标题，缺省显示 `New chat` 并以创建时间区分。 |
| 新建会话 | `POST /v1/sessions` | 沿用 `201` 和 `sessionId`。请求处理中锁定重复点击，响应不确定时刷新列表。 |
| 打开历史 | `GET /v1/sessions/{id}/events?after=&limit=` | 增补冷会话读取。活跃会话走现有读取接口，冷会话复用 Host 的只读日志读取和相同公开事件过滤。按服务端 `nextCursor` 翻页，哪怕某页公开事件为空也继续，直到游标为空。 |
| 发消息 | `POST /v1/sessions/{id}/messages` | 发送 `{ "content": "…" }`，沿用 `202` 和 `messageId`。发送给冷会话时，现有 Host 路由会恢复运行时。 |
| 流式回复 | `GET /v1/sessions/{id}/stream?after=` | 活跃会话从已读取的游标续接。使用同一页的 SSE，保留历史并实时替换回复草稿。 |
| 删除会话 | `DELETE /v1/sessions/{id}` | Host 新增处理，Controller 沿用现有转发。成功 `204`，不存在 `404`，会话忙或仍为待投递任务目标时 `409`，存储失败 `503`。 |

列表扩展以可选字段实现。Host 可以把短标题作为从日志派生的小文件放到会话目录，在收到第一条持久化人工消息后写入，打开旧会话时按已读取的历史补齐。标题缺失或写入失败时继续使用默认名，避免列表为每一行读取完整日志。 [当前列表数据结构](../internal/httpapi/session_list_params.go#L25) · [当前分页顺序](../internal/host/host_list.go#L59)

**读取冷会话。** 现有 Host 只在收到消息时自动恢复冷会话。首版应让历史读取在启动 Host 后直接读取存盘日志，不激活该会话。分页沿用现有的事件可见性规则，按原始事件序号返回游标。若文件尾部存在未完整写入的片段，读取连续有效前缀即可。当前 `ReadArtifact` 会扫描日志，接入时可按日志版本短时复用最近读取的结果，并限制缓存容量。日志变化、会话激活和删除时使相应缓存失效。已归档会话在读取历史前按删除状态过滤。 [现有冷恢复入口](../internal/httpapi/native_host_http.go#L295) · [只读日志](../harness/session/file_store.go#L201) · [公开事件过滤](../internal/httpapi/native_worker_http.go#L622)

**流式与重连。** 页面先加载持久化历史，再订阅活跃会话的 `session`、`assistant-stream-baseline` 和 `assistant-stream` 三类事件。持久事件按 `seq` 去重，发送中的用户气泡以 `messageId` 与历史合并。助手草稿按本次流的 `revision`、`attemptId` 和片段索引更新，持久化的 `assistant/message` 到达后，按轮次和步骤替换草稿，并用 `startedAfterSeq` 排除比当前草稿更早的回放事件。断线时保留已落盘内容并从最后一个持久事件游标重连，收到新 baseline 后替换旧草稿。冷会话先展示历史，提交消息成功后订阅 SSE。切换会话会释放旧连接。发送请求的响应不确定时提示先核对会话，避免自动重复投递。 [现有 SSE 实现](../internal/httpapi/native_worker_http.go#L421) · [SSE 命名事件与游标](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events/Using_server-sent_events#event_stream_format)

**删除的持久语义。** 首版做归档式删除。Host 在对应顶层会话的持久化目录中原子写入删除标记，成功后从列表和所有公开会话子路由隐藏它。这样 Docker 持久卷和 NAS 重启后仍记得删除结果。已有日志、工作区和共享附件留在存储中，删除确认框明确说明这一点。

- 删除前核验顶层会话身份，按同一生命周期顺序协调消息提交、会话激活和删除。正在回复或存在运行中任务时返回 `409`，用户可稍后删除。先阻止新的会话操作，再提交删除标记。标记落盘后清除运行时缓存并结束该会话的 SSE。对已归档的同一 ID 重复删除返回 `204`。
- 对有活跃或暂停任务明确指向该会话的情况返回 `409`，避免暗中丢弃未来提醒。Schedule 目标更新、投递与归档检查需要串行协调，已归档 ID 在自动激活和创建保留会话时也必须被识别，避免重新出现。历史回执可以继续随其任务保存。
- 只读查询、活动会话缓存和冷恢复都使用同一个 Host 删除判定。子会话、日志文件和内容寻址对象留给单独的数据清理能力。 [现有 Host 会话生命周期](../internal/hostruntime/native_host.go#L45) · [Schedule 目标处理](../harness/schedule/target.go#L47)

## 实施和验收

1. **先打通 Host 数据契约。** 扩展冷历史与删除 API，补齐删除标记、会话生命周期与调度的并发用例，同步 OpenAPI。验证打开冷会话不会触发模型调用，删除后重启不会重新列出或恢复该会话。
2. **接入前端页面。** 先用接口样本还原参考图的欢迎页，再用真实 Controller 联调。验证新建、文本发送、流式更新、历史切换和右键删除。覆盖空列表、发送失败、SSE 断线、刷新深链接、重复点击和键盘菜单。
3. **接入构建与启动。** 在现有 Controller 构建链加入 Web 资源，验收脚本启动后直接用浏览器访问。测试路由、同源限制、构建资源 404 和 SSE 不被缓冲。Docker 使用真实 Controller/Host 执行一次聊天并重启恢复。用 fake ECI 验证 Controller 按运行实例地址转发同一组请求。有可用的 ECI 环境时复验浏览器聊天。
4. **检查视觉。** 在 1440×900 和 390×844 对照参考图检查主侧栏比例、渐变层次、机器人图标、字重与输入框位置。已有会话页另检消息滚动、长内容换行、移动端菜单和键盘焦点。
