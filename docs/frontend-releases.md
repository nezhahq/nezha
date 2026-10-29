# 动态安装和切换前端

管理员进入「设置 → 前端管理」，选择「用户前端」或「管理后台前端」，输入仓库地址，读取发布版本并安装。安装完成后单独点击「启用」，新请求即使用该版本，无需重启面板。切换管理前端后浏览器会重新打开 `/dashboard/`。普通用户无权执行这些操作。

支持以下公开 HTTPS 地址：

- `https://github.com/所有者/仓库`
- `https://gitee.com/所有者/仓库`
- `https://atomgit.com/所有者/仓库`
- 其他 GitHub 兼容 API 的完整仓库地址，例如 `https://forge.example/api/v1/repos/所有者/仓库` 或 `https://git.example/api/v3/repos/所有者/仓库`，也可附带 `/releases`。

兼容 API 需要支持 `GET {仓库API}/releases?page=1&per_page=20` 和 `GET {仓库API}/releases/tags/{tag}`，返回 GitHub 格式的 `tag_name`、`assets[].name` 和 `assets[].browser_download_url`。不支持需要访问令牌的私有仓库、HTTP 或内网地址。下载重定向也必须指向公开 HTTPS 地址。

## 发布包标准

Release 必须有名为 `dist.zip` 的附件，内容全部位于 `dist/`，且包含非空的 `dist/index.html`，例如：

```text
dist.zip
└── dist/
    ├── index.html
    └── assets/
        ├── application.js
        └── application.css
```

用户前端应以 `/` 为资源基路径，管理前端应以 `/dashboard/` 为基路径，并兼容当前面板的 API。不能直接使用源码 ZIP，也不能把 `index.html` 放在 ZIP 根目录。发布列表先根据附件名、大小过滤草稿和不匹配版本；安装时再校验 ZIP 内容。预发布可以手动安装，页面会明确标记。

压缩包最多 64 MiB，解压后最多 256 MiB、10,000 个条目。禁止目录穿越、重复路径、符号链接及特殊文件。发布源提供文件大小或 SHA-256 摘要时会核对，并为每次安装记录实际 SHA-256。包结构校验无法判断第三方 JavaScript 是否可信，也不能保证其 API 兼容性；请使用自己信任的前端仓库。

## 保存和恢复

安装内容保存在配置文件同目录的 `frontends/` 下，按仓库、tag、前端类型独立存储。相同版本重复安装会复用已安装内容。失败的下载或解压不会加入可选列表，也不会覆盖正在使用的文件。容器部署需确保该配置目录持久化、可写。

当前选择沿用配置文件中的 `user_template` 和 `admin_template`，无需迁移旧配置。重启时自动恢复已安装版本和选择。内置前端始终在列表内，可以随时切回。

若第三方管理前端无法正常使用，可使用管理员 PAT（`nezha:admin:*` scope）请求 `POST /api/v1/frontend/activate`，JSON 为 `{"path":"admin-dist","is_admin":true}`；也可停止面板，将配置中的 `admin_template` 改回 `admin-dist`，再启动。用户前端对应 `user-dist`。

## 删除旧版本

安装新版本不会自动删除旧版本。切换成功后，可在前端管理中删除不再需要的自定义版本；确认后同时清理安装目录和列表记录。当前启用的版本与内置前端不可删除，需要先切换到其他版本再删除。删除无法撤销，再次使用需重新安装。

管理员也可请求 `POST /api/v1/frontend/remove`，JSON 为 `{"path":"安装记录中的 path","is_admin":false}`；管理前端使用 `is_admin:true`。接口要求管理员权限及 `nezha:admin:*` scope（使用 PAT 时），并与启用操作互斥，防止启用已删除的版本。

## 开发与发版

此功能需要配套的 `admin-frontend` 页面。正式发版时，先在管理前端仓库创建 tag 触发 release，再更新本仓库 `service/singleton/frontend-templates.yaml` 中管理前端的版本并构建面板。仅发布后端而继续嵌入旧管理前端，不会出现新的管理入口。
