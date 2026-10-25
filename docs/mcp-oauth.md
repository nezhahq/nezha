# 使用 ChatGPT 连接 Nezha MCP

Nezha 的 MCP 支持两种认证：现有客户端继续使用 `Authorization: Bearer nzp_…`，ChatGPT 等远程 MCP 客户端使用 OAuth 授权码流程。OAuth 访问令牌仅可用于 `/mcp`，不能用于面板 REST API。

## 部署与连接

1. 同时部署包含 MCP OAuth 接口的 nezha 和包含 `/dashboard/mcp/authorize` 页面的 admin-frontend。授权页由独立管理前端提供，后端没有嵌入 HTML 授权模板；旧版或第三方管理前端需要实现此路由后才能完成授权。
2. 在面板设置中启用 MCP，并把 `dashboard_host` 配置为面板对外的 HTTPS 主机名，例如 `panel.example.com` 或 `panel.example.com:8443`，不含协议、路径和末尾斜杠。OAuth 始终以 `https://<dashboard_host>` 为固定签发者，避免从请求头推导授权地址。未设置该项不影响既有 PAT 客户端。
3. 反向代理应允许访问 `/mcp`、`/.well-known/oauth-protected-resource/mcp`、`/.well-known/oauth-authorization-server`、`/oauth2/mcp/*`，以及正常的管理前端和 API 路由。公网连接需要有效 HTTPS；私有部署也可以按 OpenAI 官方说明使用 Secure MCP Tunnel，浏览器仍需能访问面板登录和授权页面。
4. 在 ChatGPT 的自定义 MCP 入口填写 `https://panel.example.com/mcp`，选择 OAuth。使用动态客户端注册（DCR），无需手工填写客户端 ID 或密钥。入口是否可用受账号和工作区策略限制。
5. 跳转到面板后登录，核对应用名称、返回地址、当前账号，选择权限和服务器，然后同意授权。应用名称是注册方自报的，并不证明其身份。读取权限默认选中，写入、删除、执行命令和“全部服务器”需要主动选择。
6. 在 ChatGPT 新聊天中选择该连接，先调用 `meta.whoami` 和 `server.list`，检查身份、权限和服务器范围，再按需使用其他工具。

官方接入说明：<https://developers.openai.com/api/docs/guides/custom-mcp-server>；认证要求：<https://developers.openai.com/plugins/build/auth>。

## 权限与撤销

OAuth 的访问和刷新凭据使用保留的 `nzo_`、`nzr_` 前缀；NAT 入口始终过滤这些凭据，包括已过期、轮换或撤销的凭据，避免把面板凭据转发到代理服务。

OAuth 权限使用现有的 `nezha:inventory:read`、`nezha:server:read/write/delete/exec` 范围，并继续检查当前用户对服务器的访问权限。管理员也必须明确批准所选权限；OAuth 不签发 `nezha:*`、`nezha:admin:*` 等通配权限。

授权记录以 `OAuth: <应用名称>` 出现在现有 API Token 列表中。删除该记录会同时阻止后续 MCP 调用和刷新。访问令牌最长一小时，授权及刷新有效期为 30 天；刷新不延长授权总期限。用户删除、密码版本变更、授权过期、关闭 MCP 或修改规范 MCP 地址也会阻止旧授权使用。关闭再开启 MCP 不会删除授权记录，需永久撤销时应删除对应记录。

管理后台「设置 → OAuth 授权」（`/dashboard/settings/mcp-oauth`）提供以下管理能力：

- 所有用户可查看自己的应用授权、权限、服务器范围、授权与到期时间、最后使用时间/IP，并撤销单项授权。
- 管理员可切换到全站授权，查看授权所属用户并撤销；「客户端管理」显示客户端 ID、名称、回调地址、授权记录数和启用状态。
- 禁用客户端会在同一事务中撤销该客户端所有用户的授权及待兑换授权码，并通知关联连接取消。重新启用不恢复旧凭据，用户必须重新授权。
- 管理接口仅接受面板登录会话，拒绝 PAT 和 MCP OAuth 令牌；修改操作沿用 CSRF 校验。即使关闭 MCP 或尚未配置 OAuth 地址，管理和撤销仍可用。

客户端禁用状态保存在 `mcp_oauth_clients.disabled`，重启后保留。禁用针对具体客户端 ID，不封禁应用提供方；动态注册仍允许创建新的 ID。禁用记录不会被 90 天未使用清理删除。授权列表展示当前数据库中的授权，已撤销项会移除，过期项可能在后续令牌交换时清理；历史工具调用仍在原审计记录中。

刷新令牌每次使用后轮换；重放旧刷新令牌会撤销该授权。客户端应串行刷新并立即保存新的令牌。授权码有效期五分钟，只能使用一次；正确凭据重放已兑换的授权码也会撤销其授权。

OAuth 签发的上传、下载 URL 同时绑定来源授权与访问凭据，其有效期不超过来源访问令牌。消费 URL 时重新校验 OAuth 状态；修改密码、轮换令牌、禁用客户端或更改规范地址后，旧 URL 不能继续用于传输。刷新后需要重新获取传输 URL。

授权信息、服务器白名单和审计身份复用现有 API Token 数据。数据库只存储客户端密钥、授权码、访问令牌和刷新令牌的哈希，不存储这些凭据的明文。刷新句柄和浏览器确认票据使用域隔离 HMAC；轮换面板 JWT 签名密钥后需要重新连接。

## 接口契约

| 接口 | 用途 |
| --- | --- |
| `GET /.well-known/oauth-protected-resource/mcp` | 固定资源地址与授权服务器发现；根路径发现地址也可用 |
| `GET /.well-known/oauth-authorization-server` | OAuth 元数据、S256、令牌与注册端点 |
| `POST /oauth2/mcp/register` | DCR，支持 `none`、`client_secret_post`、`client_secret_basic` |
| `GET /oauth2/mcp/authorize` | 校验授权请求后跳转独立管理前端，不发放授权码 |
| `GET /api/v1/mcp/oauth/consent` | JWT 登录态读取请求、可选服务器及绑定用户的确认票据 |
| `POST /api/v1/mcp/oauth/consent` | JWT + CSRF，提交明确同意或拒绝，返回回调地址 |
| `POST /oauth2/mcp/token` | 表单提交授权码兑换或刷新；必须携带固定 `resource` |
| `POST /oauth2/mcp/revoke` | 撤销同一客户端持有的访问或刷新令牌 |
| `GET /api/v1/mcp/oauth/grants` | 我的授权；管理员可用 `all=true` 查询全站授权 |
| `DELETE /api/v1/mcp/oauth/grants/:id` | 撤销自己的授权，管理员可撤销任意用户授权 |
| `GET /api/v1/mcp/oauth/clients` | 管理员查询注册客户端，不返回密钥或哈希 |
| `PUT /api/v1/mcp/oauth/clients/:id` | 管理员提交 `{"disabled":true/false}` 禁用或启用客户端 |

管理列表支持 `page`（默认 1）和 `limit`（默认 20，最大 100），使用业务 JSON 封装 `{success,data:{items,total}}`。授权状态 `active` 表示授权尚可使用或刷新，`expired` 表示授权过期，`unavailable` 表示用户凭据版本、客户端状态、MCP 开关或规范地址等条件使其不可用。

回调 URI 必须是预先注册的 HTTPS 地址并逐字匹配，不接受通配符、用户信息、片段或预填的 OAuth 结果参数。授权请求和兑换都必须携带 `resource=https://panel.example.com/mcp`。仅支持 `response_type=code` 与 `code_challenge_method=S256`，不提供隐式授权或密码授权。

匿名 OAuth 请求和注册有独立全局限流，注册最多保留 4096 个客户端；90 天未使用的注册在下次注册时清理。每位用户最多保留 30 个尚未过期的授权码、100 个有效 OAuth 授权。过期或已撤销授权的令牌记录会在后续令牌交换中清理。

## 验收范围

后端回归测试通过真实 Gin 路由覆盖发现、注册、登录会话、CSRF、明确授权、PKCE、权限与资源绑定、轮换、重放、撤销和旧 PAT 兼容；前端测试覆盖授权选择、错误处理及密码/OAuth 登录后的查询参数恢复。它们不等同于真实 ChatGPT 账号联调，部署后仍需完成第 4–6 步验收。
