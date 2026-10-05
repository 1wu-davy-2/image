# 接口契约

前端（`web/`）和后端（`server/`）之间只有这一份约定。前端是纯静态文件，除了 `/api` 和 `/health`
不依赖后端任何东西；换一套后端（比如 Go + SQLite）时照着这份文档实现即可，前端一行都不用改。

## 通用约定

- 所有接口都在 `/api` 下，请求和响应都是 JSON（`Content-Type: application/json`）。
- 登录态走 HttpOnly Cookie，不用 Authorization 头：
  - 用户端 `darkroom_user`，管理端 `darkroom_admin`，都是一次会话一个随机 token，有效期 14 天。
  - 两个会话互不相通；管理端登录不会让用户端也登录。
- 成功一律返回 `ok: true`；失败返回 `{ ok: false, error: "人话错误信息" }` 加对应的 HTTP 状态码。
  前端直接把 `error` 显示给用户，所以文案要是能看懂的中文。
- 需要登录的接口在未登录时返回 `401`；额度不够返回 `402`；被停用的账号返回 `403`。
- 前端通过 `web/config.js` 里的 `window.DARKROOM_API` 决定请求哪个源，留空表示同源。
- 前后端分开跑时（前端 6664、后端 6670）属于跨源，后端必须回 CORS 头：
  `Access-Control-Allow-Origin` 回具体来源（**不能是 `*`**）、`Access-Control-Allow-Credentials: true`，
  并且要正确响应 `OPTIONS` 预检（预检不带 Cookie，不能要求登录）。
  放行名单默认是 `http://127.0.0.1:6664` 和 `http://localhost:6664`。

## 数据模型

```
User      id, username, displayName, phone, email, quota, disabled, isAdmin,
          lastCheckinDate(YYYY-MM-DD, 北京时区), createdAt
Key       id, name, protocol, baseUrl, apiKey, userAgent,
          balance(数字或 null), balanceUnit, balanceValid(true/false/null),
          balanceUpdatedAt, balanceError, enabled, note, lastUsedAt
Settings  checkinQuota(0-1000 整数), generateCost(0-1000 整数)
Admin     username, 密码哈希(scrypt + salt)
Generation  id, username, displayName, prompt, protocol, model, sizeLabel, channel,
          taskId, isPublic, mine, createdAt, images[{position, mime}]
```

`protocol` 四选一：`gpt`、`nano`、`gemini-official`、`gemini-batch`。

## 用户端

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/auth/register` | 注册并登录。body `{username, displayName?, password, phone, email}`，字段规则见下 |
| `POST` | `/api/auth/login` | 登录。body `{account, password}`，`account` 名称或邮箱都收。被停用的账号返回 403 |
| `POST` | `/api/auth/logout` | 退出，清 Cookie |
| `GET` | `/api/me` | 未登录也返回 200：`{ok, user: null 或用户对象, checkinQuota, generateCost}` |
| `POST` | `/api/checkin` | 签到。同一天重复签到返回 400 |
| `POST` | `/api/me/password` | 改自己的密码。body `{oldPassword, newPassword}`，原密码错返回 401 |
| `PATCH` | `/api/me` | 改中文名。body `{displayName}`，最多 24 字，超了 400 |
| `POST` | `/api/generate` | 生图，见下 |
| `GET` | `/api/generations` | 自己的作品集。`limit`（默认 24，上限 100）、`offset` |
| `GET` | `/api/generations/{id}` | 作品详情。自己的，或已公开的；其余一律 404 |
| `PATCH` | `/api/generations/{id}` | 公开 / 取消公开。body `{isPublic}`，不是布尔值返回 400 |
| `DELETE` | `/api/generations/{id}` | 删除自己的作品 |
| `GET` | `/api/generations/{id}/images/{position}` | 作品图片，原始字节。字节还没拉回来时 302 到上游链接 |
| `GET` | `/api/works` | 公开作品。不登录也能看，登录了会标出 `mine` |
| `/api/batches...` | | 批量生图，见下 |

### 注册字段规则

| 字段 | 必填 | 规则 |
| --- | --- | --- |
| `username` | 是 | 2–20 位，只能英文字母和数字。不区分大小写唯一，重复返回 409 |
| `displayName` | 否 | 中文名，最多 24 个字 |
| `password` | 是 | 至少 8 位，最多 72 位 |
| `phone` | 是 | 11 位大陆手机号（1 开头、第二位 3–9）。不查重 |
| `email` | 是 | 基本格式校验。不区分大小写唯一，重复返回 409；可当 `account` 登录 |

字段不合法一律 400，文案里带具体原因。

### POST /api/generate

```json
{
  "protocol": "gpt",
  "model": "gpt-image-2.5-flare",
  "prompt": "……",
  "mode": "generate",
  "size": "1024x1024",
  "quality": "high"
}
```

- `mode` 是 `generate` 或 `edit`。`edit` 必须再带参考图：`image: {mime, data, name}`（base64）
  或 `imageUrl`（https）。
- 蒙版可选：`mask`（同上结构）或 `maskUrl`。只在 `edit` 下有效，`gemini-official` 不支持。
- `protocol` 是 `gpt` 时用 `size` + `quality`；是 `nano` / `gemini-official` 时改用
  `aspectRatio`（`1:1`/`3:2`/`2:3`/`4:3`/`3:4`/`16:9`/`9:16`）+ `imageSize`（`1K`/`2K`/`4K`）。

返回：

```json
{ "ok": true, "channel": "async", "taskId": "imgtask_...",
  "images": [{ "url": "https://..." }], "quota": 41 }
```

`images` 的元素要么是 `{url}`，要么是 `{b64, mime}`。`channel` 是 `async`/`sync`/`chat`/`gemini`，
前端只用来提示走了哪条链路。

**额度**：进入时先按 `generateCost` 扣，生成失败要把扣掉的加回去，并在错误响应里带上 `quota` 字段。
挑 Key 失败、上游报错都算失败。

## 批量生图

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/batches` | 提交。body `{model, provider, image_size, response_mime_type, items}` |
| `GET` | `/api/batches` | 列表，query 原样透传给上游 |
| `GET` | `/api/batches/models` | 可用模型 |
| `GET` | `/api/batches/{id}` | 任务详情 |
| `GET` | `/api/batches/{id}/items` | 条目 |
| `GET` | `/api/batches/{id}/items/{custom_id}/content` | 单项内容，**原始字节**，不包 JSON |
| `GET` | `/api/batches/{id}/download` | 下载 ZIP，**原始字节**，透传 Content-Type 和 Content-Disposition |
| `POST` | `/api/batches/{id}/cancel` | 取消 |
| `DELETE` | `/api/batches/{id}/outputs` | 删除输出 |
| `DELETE` | `/api/batches/{id}` | 删除记录 |

- `items` 每项是 `{custom_id, prompt, output_count}`，`custom_id` 只能用字母数字下划线点和短横线，
  `output_count` 是 1-4。单个任务最多 200 个条目、200 个输出（所有 `output_count` 之和）。
- 提交按 `generateCost × 输出总数` 扣额度，**提交失败要全额退回**；上游受理之后不再退。
- 除提交外的所有操作都不扣额度，但同样需要挑一把 `gemini-batch` 的 Key。
- 非原始字节的接口统一返回 `{ok: true, result: <上游原样返回的 JSON>}`。

## 管理端

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/admin/login` | body `{username, password}`。账号或密码错都是 401，文案不区分 |
| `POST` | `/api/admin/logout` | 退出 |
| `GET` | `/api/admin/state` | `{ok, settings, keys[], users[], checkinDate}` |
| `PUT` | `/api/admin/settings` | body `{checkinQuota, generateCost}` |
| `POST` | `/api/admin/password` | body `{oldPassword, newPassword}` |
| `POST` | `/api/admin/keys` | 新增或修改 Key，带 `id` 是修改。`apiKey` 留空表示不改 |
| `DELETE` | `/api/admin/keys/{id}` | 删除 Key |
| `POST` | `/api/admin/keys/{id}/balance` | 查余额，见下 |
| `PATCH` | `/api/admin/users/{id}` | body 可含 `quota` 和/或 `disabled` |
| `POST` | `/api/admin/users/{id}/password` | 重置该用户密码，同时踢掉他的所有会话 |
| `DELETE` | `/api/admin/users/{id}` | 删除用户 |

停用、重置密码、删除都要立刻清掉该用户已有的会话（旧 Cookie 马上失效）。

## 后端要做的事

这部分不是 HTTP 契约，是后端内部必须实现的行为。

### 挑 Key

从该 `protocol` 下挑一把：`enabled !== false`、`balanceValid !== false`（中转说过失效的不要）、
有 `apiKey`。优先余额大于 0 的，按余额从大到小、最近最少用；余额未知的排最后。挑不到就报错。

### 调用上游

中转地址只允许 `uuapi.io`、`uuapi.net`、`uuapi.shop`、`uuapi.cc` 的 https。
所有请求带 `Authorization: Bearer <apiKey>`，以及该 Key 配的 `User-Agent`
（没配就用 `darkroom/1.0 (local image studio)`）。上游超时 120 秒。

- **gpt**：`POST {origin}/v1/images/generations/async`（编辑用 `/edits/async`），
  202 拿 `task_id` 后轮询 `GET /v1/images/tasks/{task_id}`，最多 180 秒，按 `Retry-After` 间隔。
  异步接口不存在（404/405/501）就退到同步 `/v1/images/generations`。
- **nano**：同上；生图接口不收这个模型时改走 `POST /v1/chat/completions`，
  从 `choices[0].message.content` 里抠图片。
- **gemini-official**：`POST {origin}/v1beta/models/{model}:generateContent`，
  正文 `contents[].parts`，`generationConfig.responseModalities` 含 `IMAGE`，
  从 `candidates[].content.parts[].inlineData.data` 读 base64。
- **gemini-batch**：`{origin}/v1/images/batches` 一族，见上面表格。

参考图和蒙版只收 PNG/JPEG/WebP，各自上限 20MB。用 URL 时要挡内网和本机地址，
跟随重定向最多 3 跳。参考图和蒙版一个是文件一个是 URL 时，把 URL 那一半下下来，
统一按 multipart 发（上游只有「两个文件」和「两个 URL」两种形状）。

### 查余额

只打 `GET {中转地址去掉 /v1}/v1/usage`，带 Bearer。解析规则和 cc-switch 导入中转站时一致：

| 字段 | 取值顺序 |
| --- | --- |
| 余额 | `remaining` → `quota.remaining` → `balance` |
| 单位 | `unit` → `quota.unit` → `USD` |
| 是否有效 | `is_active` → `isValid` → 默认有效 |

按 `??` 语义取值（余额是 0 要当成 0，不能跳过）。只有拿到明确的布尔值才写 `balanceValid`。

## 其他

- `GET /health` 返回 `{ok: true}`，不需要登录。
- 静态文件由后端一起提供（`web/` 目录），找不到的路径返回 404。
- 除 `/api` 和 `/health` 外的方法一律 405。
