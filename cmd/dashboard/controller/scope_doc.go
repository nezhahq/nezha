// Package controller — scope reference table for REST + MCP.
//
// Each REST endpoint under /api/v1/* and each MCP tool under /mcp requires a
// specific scope when authenticated via PAT (`Authorization: Bearer nzp_*`).
// JWT-authenticated requests skip scope enforcement.
//
// This file is the authoritative human + LLM-readable index. The actual
// enforcement lives in controller.go (REST) and mcp_tools_*.go (MCP). When
// you change an endpoint's scope requirement, update this table.
//
// # Scope naming
//
//	nezha:{resource}:{verb}
//	  resource: inventory | server | service | alertrule | cron | ddns | nat |
//	            notification | notification-group | transfer | admin
//	  verb:     read | write | delete | exec
//
//	inventory vs server：inventory 管“能看到/能删哪些机器”（列出 server /
//	server-group、删除 server / server-group、MCP server.list）；server 管对
//	已知机器的运行态操作（exec / 文件读写 / 编辑配置 / metrics / server.get）。
//
//	nezha:*               Admin-only superuser
//	nezha:admin:*         Admin-only user/waf/setting/online-user management
//	nezha:<res>:*         All actions on a resource
//
// # MCP tools (POST /mcp tools/call)
//
//	meta.whoami           — (any scope)
//	server.list           nezha:inventory:read
//	server.get            nezha:server:read
//	server.exec           nezha:server:exec
//	fs.list               nezha:server:read
//	fs.read               nezha:server:read
//	fs.write              nezha:server:write
//	fs.delete             nezha:server:delete
//	fs.download_url       nezha:server:read
//	fs.upload_url         nezha:server:write
//
// # REST endpoints (PAT required scope)
//
//	GET    /api/v1/server                            nezha:inventory:read
//	PATCH  /api/v1/server/{id}                       nezha:server:write
//	GET    /api/v1/server/config/{id}                nezha:server:write
//	POST   /api/v1/server/config                     nezha:server:write
//	POST   /api/v1/batch-delete/server               nezha:inventory:delete
//	POST   /api/v1/batch-move/server                 nezha:server:write
//	POST   /api/v1/force-update/server               nezha:server:write
//	POST   /api/v1/server-group                      nezha:server:write
//	PATCH  /api/v1/server-group/{id}                 nezha:server:write
//	POST   /api/v1/batch-delete/server-group         nezha:inventory:delete
//	POST   /api/v1/terminal                          nezha:server:exec
//	GET    /api/v1/ws/terminal/{id}                  nezha:server:exec
//	POST   /api/v1/file                              nezha:server:read+write+delete
//	GET    /api/v1/ws/file/{id}                      nezha:server:read+write+delete
//	GET    /api/v1/ws/server                         nezha:inventory:read
//	GET    /api/v1/server-group                      nezha:inventory:read
//	GET    /api/v1/service                           nezha:service:read
//	GET    /api/v1/service/server                    nezha:service:read
//	GET    /api/v1/service/{id}/history              nezha:service:read
//	GET    /api/v1/server/{id}/service               nezha:service:read
//	GET    /api/v1/server/{id}/metrics               nezha:server:read
//
//	GET    /api/v1/transfer                          nezha:transfer:read
//	POST   /api/v1/transfer/{id}/cancel              nezha:transfer:write
//	POST   /api/v1/transfer/{id}/retry               nezha:transfer:write
//	GET    /api/v1/ws/transfer                       nezha:transfer:read
//
//	GET    /api/v1/service/list                      nezha:service:read
//	POST   /api/v1/service                           nezha:service:write
//	PATCH  /api/v1/service/{id}                      nezha:service:write
//	POST   /api/v1/batch-delete/service              nezha:service:delete
//
//	GET    /api/v1/alert-rule                        nezha:alertrule:read
//	POST   /api/v1/alert-rule                        nezha:alertrule:write
//	PATCH  /api/v1/alert-rule/{id}                   nezha:alertrule:write
//	POST   /api/v1/batch-delete/alert-rule           nezha:alertrule:delete
//
//	GET    /api/v1/cron                              nezha:cron:read
//	POST   /api/v1/cron                              nezha:cron:write
//	PATCH  /api/v1/cron/{id}                         nezha:cron:write
//	POST   /api/v1/cron/{id}/manual                  nezha:cron:exec
//	POST   /api/v1/batch-delete/cron                 nezha:cron:delete
//
//	GET    /api/v1/ddns                              nezha:ddns:read
//	GET    /api/v1/ddns/providers                    nezha:ddns:read
//	POST   /api/v1/ddns                              nezha:ddns:write
//	PATCH  /api/v1/ddns/{id}                         nezha:ddns:write
//	POST   /api/v1/batch-delete/ddns                 nezha:ddns:delete
//
//	GET    /api/v1/nat                               nezha:nat:read
//	POST   /api/v1/nat                               nezha:nat:write
//	PATCH  /api/v1/nat/{id}                          nezha:nat:write
//	POST   /api/v1/batch-delete/nat                  nezha:nat:delete
//
//	GET    /api/v1/notification                      nezha:notification:read
//	POST   /api/v1/notification                      nezha:notification:write
//	PATCH  /api/v1/notification/{id}                 nezha:notification:write
//	POST   /api/v1/batch-delete/notification         nezha:notification:delete
//
//	GET    /api/v1/notification-group                nezha:notification-group:read
//	POST   /api/v1/notification-group                nezha:notification-group:write
//	PATCH  /api/v1/notification-group/{id}           nezha:notification-group:write
//	POST   /api/v1/batch-delete/notification-group   nezha:notification-group:delete
//
//	GET    /api/v1/user                              nezha:admin:*
//	POST   /api/v1/user                              nezha:admin:*
//	POST   /api/v1/batch-delete/user                 nezha:admin:*
//	GET    /api/v1/waf                               nezha:admin:*
//	POST   /api/v1/batch-delete/waf                  nezha:admin:*
//	GET    /api/v1/online-user                       nezha:admin:*
//	POST   /api/v1/online-user/batch-block           nezha:admin:*
//	PATCH  /api/v1/setting                           nezha:admin:*
//	POST   /api/v1/maintenance                       nezha:admin:*
//
// # Endpoints permanently forbidden to PAT
//
// These are personal-account-management endpoints; a PAT must never call them
// (would allow self-elevation chains: PAT → mint stronger PAT → ...).
// `restPATForbiddenMiddleware` returns 403 to PAT-authenticated requests.
//
//	POST   /api/v1/refresh-token
//	GET    /api/v1/profile
//	POST   /api/v1/profile
//	POST   /api/v1/oauth2/{provider}/unbind
//	GET    /api/v1/api-tokens
//	POST   /api/v1/api-tokens
//	DELETE /api/v1/api-tokens/{id}
//
// Backups hold every credential of the dashboard; the backup endpoints are
// likewise refused to PAT and additionally require an administrator:
//
//	GET    /api/v1/backup
//	POST   /api/v1/backup
//	GET    /api/v1/backup/config
//	PATCH  /api/v1/backup/config
//	POST   /api/v1/backup/config/test
//	POST   /api/v1/backup/password
//	PATCH  /api/v1/backup/password
//	POST   /api/v1/backup/password/reset
//	GET    /api/v1/backup/{name}/download
//	POST   /api/v1/batch-delete/backup
//	POST   /api/v1/backup/restore/prepare
//	POST   /api/v1/backup/restore/apply
//	DELETE /api/v1/backup/restore/{token}
//	GET    /api/v1/backup/restore/status
package controller
