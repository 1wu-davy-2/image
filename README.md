# 暗房 · GPT / Gemini 生图工作台

一个跑在本机的生图工作台。Node 起一个服务，浏览器打开就能用：用户注册登录、每天签到领额度、
消耗额度生图；管理员在管理端维护中转站（UU API）的 Key 和余额，服务端按调用方式自动挑一把
还有余额的 Key 去请求上游。

前端是原生 HTML / CSS / JS，没有构建步骤，整个项目也没有第三方依赖。

![生图工作台](docs/studio.png)

## 能做什么

三种调用方式，覆盖中转站上常见的几条生图链路：

- **GPT 生图** —— `POST /v1/images/generations`，图生图走 `/v1/images/edits`。
  先试异步接口再轮询 `/v1/images/tasks/{task_id}`；异步接口不存在时降级到同步接口，
  同步接口也不认时改走 `/v1/chat/completions` 对话生图。
- **Nano Banana 香蕉生图** —— 同一套生图接口，模型换成 `gemini-2.5-flash-image`、
  `gemini-3-pro-image` 等；生图接口不收这个模型时自动改走对话生图。
- **GEMINI官方直连-带生图** —— `POST /v1beta/models/{model}:generateContent`，正文是 Gemini
  官方格式，画幅和分辨率放在 `generationConfig.imageConfig`，参考图以 `inlineData` 放进 `contents`。

其余功能：

- 文生图 / 图生图，参考图可以上传（PNG、JPEG、WebP，小于 8MB），也可以填一个 https 图片地址。
- 账号体系：注册登录、每天签到领额度、每次生图扣额度，生图失败自动把额度退回去。
- Key 池：按调用方式分组，优先挑余额多的、最近没怎么用过的 Key；余额为 0 或停用的不会被选中。
- 生成记录留在浏览器 localStorage 里（最近 16 条），服务端不存图片。

## 快速开始

需要 Node 18 或更高版本。

```bash
node server.js          # 或者 npm start
```

然后打开 http://127.0.0.1:3780 。

首次启动会随机生成一个管理密码，打印在控制台，同时写进 `data/initial-admin-password.txt`
（在管理端改过密码后这个文件会自动删掉）。想自己指定就用 `ADMIN_PASSWORD` 环境变量：

```bash
PORT=3780 ADMIN_PASSWORD=your-password node server.js
```

服务只监听 `127.0.0.1`，不对外网开放。

## 上手顺序

1. 打开 http://127.0.0.1:3780/admin ，用初始密码进入管理端。
2. 在「生图 Key」里加一把 Key：选类型，填中转地址和 API Key。GPT / 香蕉填 `https://uuapi.io/v1`，
   官方直连填 `https://uuapi.io`。保存后点「刷新余额」，确认能查到余额。
3. 回到 http://127.0.0.1:3780 ，注册一个账号，点「签到领额度」。
4. 选调用方式、模型，写好描述，点「生成」。

## 管理端

![管理端](docs/admin.png)

- **签到规则**：每天签到领多少额度、每次生图扣多少额度。
- **生图 Key**：增删改查、启用停用、刷新余额。中转地址只接受 `uuapi.io`、`uuapi.net`、
  `uuapi.shop`、`uuapi.cc` 四个域名下的 https 地址。
- **用户额度**：直接改某个用户的额度。

手机上是这样：

<img src="docs/studio-mobile.png" alt="手机端" width="320" />

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `3780` | 监听端口 |
| `ADMIN_PASSWORD` | 随机生成 | 管理端密码；设了之后每次启动都会对齐成这个值 |

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/auth/register` | 注册并登录 |
| `POST` | `/api/auth/login` | 登录 |
| `POST` | `/api/auth/logout` | 退出 |
| `GET` | `/api/me` | 当前用户、签到额度、每次生图消耗 |
| `POST` | `/api/checkin` | 签到领额度 |
| `POST` | `/api/generate` | 生图（先扣额度，失败退回） |
| `POST` | `/api/admin/login` | 管理端登录 |
| `GET` | `/api/admin/state` | 签到规则、Key 列表、用户列表 |
| `PUT` | `/api/admin/settings` | 改签到规则 |
| `POST` | `/api/admin/password` | 改管理密码 |
| `POST` | `/api/admin/keys` | 新增 / 修改 Key |
| `DELETE` | `/api/admin/keys/{id}` | 删除 Key |
| `POST` | `/api/admin/keys/{id}/balance` | 向中转站查询并记录余额 |
| `PATCH` | `/api/admin/users/{id}` | 改用户额度 |
| `GET` | `/health` | 健康检查 |

`POST /api/generate` 的正文（GPT 生图）：

```json
{
  "protocol": "gpt",
  "model": "gpt-image-2.5-flare",
  "prompt": "冬夜里的灯塔，海面翻着白浪，胶片颗粒，窗口是暖色的。",
  "mode": "generate",
  "size": "1024x1024",
  "quality": "high"
}
```

`protocol` 换成 `nano` 或 `gemini-official` 时，用 `aspectRatio`（`1:1`、`3:2`、`2:3`、`4:3`、
`3:4`、`16:9`、`9:16`）加 `imageSize`（`1K`、`2K`、`4K`）代替 `size` 和 `quality`。
图生图把 `mode` 改成 `edit`，再带上 `image: { mime, data, name }`（base64）或 `imageUrl`。

返回：

```json
{ "ok": true, "channel": "async", "taskId": "task_8f21c4", "images": [{ "url": "https://..." }], "quota": 41 }
```

`images` 里的元素要么是 `{ url }`，要么是 `{ b64, mime }`。`channel` 是 `async` / `sync` /
`chat` / `gemini`，表示这次实际走的是哪条链路。

## 目录

```
server.js            HTTP 服务、路由、上游调用与降级
store.js             用户、会话、Key、额度的读写
public/index.html    用户端页面
public/app.js        用户端逻辑
public/styles.css    用户端样式
public/admin.html    管理端页面
public/admin.js      管理端逻辑
public/admin.css     管理端样式
docs/                README 里的截图
data/                运行时数据（已 gitignore）
```

## 说明

- 数据都写在 `data/store.json` 里，API Key 是明文存的。别把这个目录提交上去，也别把服务开到公网。
- 会话用 HttpOnly Cookie，有效期 14 天。管理端和用户端的会话是分开的。
- 请求上游超时 120 秒，异步任务最多轮询 180 秒；请求体上限 14MB，参考图上限 8MB。
- README 里的截图为了展示结果区，用占位图填了生成结果——截图时没有真的调上游接口，界面本身是实际的。
