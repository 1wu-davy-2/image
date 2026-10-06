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
Key       id, name, protocol, modelType, baseUrl, apiKey, userAgent,
          balance(数字或 null), balanceUnit, balanceValid(true/false/null),
          balanceUpdatedAt, balanceError, models[{id, name}], modelsUpdatedAt, modelsError,
          enabled, note, lastUsedAt
ModelType type, label, defaultModel, multiplier
Settings  checkinMin, checkinMax(各是 0-1000 整数，且 min ≤ max), generateCost(0-1000 整数)
Admin     username, 密码哈希(scrypt + salt)
Checkin   id, day(YYYY-MM-DD), amount, createdAt
Generation  id, username, displayName, prompt, protocol, model, sizeLabel, channel,
          taskId, isPublic, mine, createdAt, images[{position, mime}]
```

签到额度是个**闭区间** `[checkinMin, checkinMax]`，每次签到在这段里随机取一个整数。
两个值相等就是固定额度。

### 生图类型与调用方式

这是两个维度，**并存**：

- `modelType` 是**给用户挑的**：`gpt`、`gemini`、`grok` 三选一。用户只挑这个，
  不挑具体模型——模型走这个类型的 `defaultModel`。
- `protocol` 管**请求怎么发**：`gpt`、`nano`、`gemini-official`、`gemini-batch`。
  它是 Key 的属性，用户看不见。挑中哪把 Key，这次就用那把的调用方式。

一把 Key = 一个类型 + 一个调用方式，同一个类型下面可以挂不同调用方式的 Key。

三个类型是固定的，管理端只能改每行的 `defaultModel` 和 `multiplier`，不能增删。
`multiplier` 是扣额度的倍率（稳定版贵一点，比如 1.5）。

## 用户端

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/auth/register` | 注册并登录。body `{username, displayName?, password, phone, email}`，字段规则见下 |
| `POST` | `/api/auth/login` | 登录。body `{account, password}`，`account` 名称或邮箱都收。被停用的账号返回 403 |
| `POST` | `/api/auth/logout` | 退出，清 Cookie |
| `GET` | `/api/me` | 未登录也返回 200：`{ok, user: null 或用户对象, checkinMin, checkinMax, generateCost}` |
| `POST` | `/api/checkin` | 签到。同一天重复签到返回 400。返回 `{ok, amount, user}`，`amount` 是这次随机到的额度 |
| `GET` | `/api/checkins` | 自己的签到记录，按时间倒序。`limit`（默认 24，上限 100）、`offset`，返回 `{ok, items[], total}` |
| `GET` | `/api/models` | 各调用方式下**中转站实际支持**的模型，见下 |
| `GET` | `/api/types` | 生图类型列表，用户端挑类型用，见下 |
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

用户端的走法：只给 `type`，不给 `model`。

```json
{
  "type": "gpt",
  "prompt": "……",
  "mode": "generate",
  "size": "2048x2048",
  "quality": "medium",
  "aspectRatio": "1:1",
  "imageSize": "2K"
}
```

老走法还留着：给 `protocol` + `model`，倍率按 1 算。两种都不给按 `protocol` 的默认值走。

- 给 `type` 时：模型用这个类型的 `defaultModel`，调用方式用**挑中的那把 Key 的**
  `protocol`，费用 = `generateCost × multiplier`（向上取整，至少 1）。
- 因为调用方式是挑完 Key 才知道的，前端**两套尺寸字段都要发**（`size` + `quality`、
  `aspectRatio` + `imageSize`），服务端按挑中的调用方式取用得上的那套。
- `mode` 是 `generate` 或 `edit`。`edit` 必须再带参考图：`image: {mime, data, name}`（base64）
  或 `imageUrl`（https）。
- 蒙版可选：`mask`（同上结构）或 `maskUrl`。只在 `edit` 下有效，`gemini-official` 不支持。
- 调用方式是 `gpt` 时用 `size` + `quality`；是 `nano` / `gemini-official` 时改用
  `aspectRatio`（`1:1`/`3:2`/`2:3`/`4:3`/`3:4`/`16:9`/`9:16`）+ `imageSize`（`1K`/`2K`/`4K`）。
- `size` 只收具体像素（`2048x1536`）。**发 `2K` 这种档位写法会被上游打回**
  「图片尺寸无效」。上限是单边 8192 像素、总像素 64Mi（这是上游自己报的）。
  界面上那套「1K / 2K / 4K × 画幅」是前端自己换算成像素再发的：
  长边定档（1K→1024、2K→2048、4K→4096），短边按画幅比例算。

返回：

```json
{ "ok": true, "channel": "async", "taskId": "imgtask_...",
  "images": [{ "url": "https://..." }], "quota": 41 }
```

`images` 的元素要么是 `{url}`，要么是 `{b64, mime}`。`channel` 是 `async`/`sync`/`chat`/`gemini`，
前端只用来提示走了哪条链路。

**额度**：进入时先按 `generateCost × multiplier`（向上取整）扣，生成失败要把扣掉的加回去，
并在错误响应里带上 `quota` 字段。挑 Key 失败、规格不合法、上游报错都算失败。
向上取整是因为额度是整数：1.5 倍的模型按 1 倍收，倍率就等于没有；向下取整又会少扣。

**报错文案**：上游的原文照转，但认得出「模型不在分组里」「余额不足」这两种时，
要在后面补一句中文说清下一步干什么——只透英文原文，用户不知道该换模型还是换 Key。

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
| `PUT` | `/api/admin/settings` | body `{checkinMin, checkinMax, generateCost}`。min > max 返回 400 |
| `POST` | `/api/admin/password` | body `{oldPassword, newPassword}` |
| `POST` | `/api/admin/keys` | 新增或修改 Key，带 `id` 是修改。`apiKey` 留空表示不改 |
| `DELETE` | `/api/admin/keys/{id}` | 删除 Key |
| `POST` | `/api/admin/keys/{id}/balance` | 查余额，见下 |
| `POST` | `/api/admin/keys/{id}/models` | 拉这把 Key 能用的模型，见下 |
| `PUT` | `/api/admin/types/{type}` | body `{defaultModel, multiplier}`。倍率限 0.1–100 |
| `PATCH` | `/api/admin/users/{id}` | body 可含 `quota` 和/或 `disabled` |
| `POST` | `/api/admin/users/{id}/password` | 重置该用户密码，同时踢掉他的所有会话 |
| `DELETE` | `/api/admin/users/{id}` | 删除用户 |

停用、重置密码、删除都要立刻清掉该用户已有的会话（旧 Cookie 马上失效）。

## 后端要做的事

这部分不是 HTTP 契约，是后端内部必须实现的行为。

### 挑 Key

批量生图那套按 `protocol` 挑；用户端生图按 `modelType` 挑。共同的门槛：
`enabled !== false`、`balanceValid !== false`（中转说过失效的不要）、有 `apiKey`、
余额大于 0（余额未知的留着）。挑不到就报错，**报错前不能扣额度**。

按类型挑时还要分三档，档内按余额从大到小、最近最少用：

1. `models[]` 里确实有这个类型的 `defaultModel`
2. `models[]` 是空的（还没拉过，属于「不知道」，不能当成「不支持」排除掉）
3. 拉了列表但里面没有这个模型

第三档留着兜底：模型列表可能是旧的，硬排除会让用户直接生不了图。但只要前两档有人，
就不用它——中转站按分组给模型，配了 Key 不代表这把 Key 的账号支持这个模型，
挑错了用户收到的是 `Model "x" is not supported by any configured account in this group`
这种看不懂的报错。

### GET /api/types

```json
{ "ok": true, "types": [
  { "type": "gpt", "label": "GPT", "defaultModel": "gpt-image-2.5", "multiplier": 1.5,
    "keys": 2, "usable": true, "modelKnown": true } ] }
```

- 固定三个类型，但**一把 Key 都没挂的类型不列出来**——用户端只看到空下拉框没意义。
- `usable`：这个类型下面至少有一把能用的 Key。
- `modelKnown`：这些 Key 里至少有一把拉过模型列表、且列表里有 `defaultModel`。
  管理端把默认模型配错了，用户端当场就能提示，不用等生图报错。

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

### 拉模型

`POST /api/admin/keys/{id}/models` 打 `GET {中转地址}/v1/models`，带同一套 Bearer 和 User-Agent。
正文里 `data[]`（有的站是 `models[]`）每项取 `id`（或 `name` / `model`），
显示名取 `display_name`（或 `displayName`），没有就退回 id。没有 id 的条目直接丢。
一个都没解析出来算 502。失败只记 `modelsError`，**不清掉上一次拉到的列表**。

`GET /api/models` 把各把 Key 拉到的模型按 `protocol` 汇总，返回：

```json
{ "ok": true, "models": { "gpt": [ { "id": "gpt-image-2.5", "name": "gpt-image-2.5",
                                   "keys": 1, "available": true } ] } }
```

这个接口是**按调用方式**汇总的，管理端排查用；用户端挑的是 `/api/types`。

- 只统计 `enabled`、有 `apiKey`、且拉过模型的 Key；停用的整把不算数。
- `available`：至少有一把「启用 + `balanceValid !== false` + 余额大于 0（或未知）」的 Key 挂着。
  不可用的**照样列出来**，只是标成 `available: false`——直接藏掉的话用户只会看到空下拉框，
  分不清是没拉过模型还是 Key 出了问题。
- 可用的排前面，同组按 id 排，顺序稳定。
- 没拉过任何模型的调用方式不会出现在结果里。

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
