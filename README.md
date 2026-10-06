# 暗房 · GPT / Gemini 生图工作台

一个跑在本机的生图工作台。后端是 Go 起的一个服务，浏览器打开就能用：用户注册登录、每天签到领额度、
消耗额度生图；管理员在管理端维护中转站（UU API）的 Key 和余额，服务端按调用方式自动挑一把
还有余额的 Key 去请求上游。

前端是原生 HTML / CSS / JS，没有构建步骤、没有第三方依赖。页面风格沿用「飞天」那套
纸底墨字的样式：Noto Serif SC 标题、青绿点缀、云纹分隔。

![生图工作台](docs/studio.png)

## 能做什么

四种调用方式，覆盖中转站上常见的几条生图链路：

- **GPT 生图** —— `POST /v1/images/generations`，图生图走 `/v1/images/edits`。
  先试异步接口再轮询 `/v1/images/tasks/{task_id}`；异步接口不存在时降级到同步接口，
  同步接口也不认时改走 `/v1/chat/completions` 对话生图。
- **Nano Banana 香蕉生图** —— 同一套生图接口，模型换成 `gemini-2.5-flash-image`、
  `gemini-3-pro-image` 等；生图接口不收这个模型时自动改走对话生图。
- **GEMINI官方直连-带生图** —— `POST /v1beta/models/{model}:generateContent`，正文是 Gemini
  官方格式，画幅和分辨率放在 `generationConfig.imageConfig`，参考图以 `inlineData` 放进 `contents`。
- **Gemini 批量生图** —— `POST /v1/images/batches` 一次提交多条提示词，之后查任务、看条目、
  下载 ZIP、取消、删除。这一族接口在用户端有独立面板。

其余功能：

- 文生图 / 图生图，参考图可以上传（PNG、JPEG、WebP，小于 64MB），也可以填一个 https 图片地址。
- 图生图可以带**蒙版**，指定只重绘哪一块（Gemini 官方直连不支持蒙版）。
- 账号体系：注册登录、每天签到领额度、每次生图扣额度，生图失败自动把额度退回去；
  用户可以改自己的密码。
- Key 池：按调用方式分组，优先挑余额多的、最近没怎么用过的 Key；余额为 0、停用的、
  以及中转站报告已失效的 Key 都不会被选中。
- 每把 Key 可以单独配请求头 `User-Agent`，应付中转站的外接策略。
- 生成记录存在服务端的 `generations` 表里，图片一并存成 BLOB，所以在「作品集」里翻得到，
  上游链接过期也不影响。可以挑几张设成公开，出现在「公开作品」里。

## 快速开始

前端要 Node 18 或更高版本（静态服务用的就是它），后端要 Go 1.26 或更高版本。

前端和后端是两个独立的进程，分别起。这里用 16666 / 18888，和 `web/config.js` 里写死的后端
端口对上（代码里的默认值其实是 6664 / 6670，不改 `config.js` 的话就得按默认值跑）：

```bash
PORT=16666 node web-server.js      # 前端静态服务，127.0.0.1:16666

cd server-go
PORT=18888 \
CORS_ORIGINS=http://127.0.0.1:16666,http://localhost:16666 \
go run .                           # 后端，127.0.0.1:18888
```

**数据库**：不设 `DB_HOST` 就用本地 SQLite 文件（`DATA_FILE`，默认 `../data/darkroom.db`）；
设了就走 MariaDB：

```bash
DB_HOST=101.43.75.72 DB_PORT=3306 DB_NAME=image \
DB_USER=image DB_PASSWORD='...' \
PORT=18888 CORS_ORIGINS=... go run .
```

`CORS_ORIGINS` 那一行不能省：前端在 16666、后端在 18888，属于跨源，不放行的话
浏览器会直接报 **Failed to fetch**，看起来像后端挂了，其实是被 CORS 挡了。

然后打开 **http://127.0.0.1:16666** 。

管理端默认账号是 **`admin` / `admin@123`**，两个服务都只监听 `127.0.0.1`，不对外网开放。
登录后请到管理端把密码改掉。

想换账号密码就用环境变量，设了 `ADMIN_PASSWORD` 之后每次启动都会把管理端对齐到这个密码，
忘了密码时也能用它找回来：

```bash
ADMIN_USER=admin ADMIN_PASSWORD=your-password go run .
```

> **别用 6665–6669 这几个端口。** 那是 IRC 的保留段，Chrome 和 Edge 会直接拒绝连接
> （`ERR_UNSAFE_PORT`），curl 却一切正常，很容易查半天。前端默认 6664、后端默认 6670
> 就是为了避开这一段。

## 工作台

登录之后是五页，左边一条菜单栏（同一组链接在顶栏中间也放了一份，窄屏只留侧栏那份），
右上角依次是额度、中转站余额和用户菜单（显示中文名，没填就显示名称）：

| 页面 | 干什么 |
| --- | --- |
| 首页 · 签到 | 看额度、签到领额度、翻签到记录，外加最近几张作品 |
| 公开作品 | 所有人公开出来的图。不登录也能看。每张下面有「复制提示词」，点图放大看（页内灯箱，不跳新标签页） |
| 创作 | 「画面」和「批量生图」两个标签页，默认画面。挑**生图类型**（GPT / GEMINI / GROK），模型默认走管理端配的那个，也可以自己换 |

尺寸按「画幅 × 分辨率」选：画幅 `1:1` / `4:3` / `3:4` / `16:9`，分辨率 `1K` / `2K` / `4K`。
长边定档（1K→1024、2K→2048、4K→4096），短边按比例算，所以 `2K · 4:3` 就是 `2048x1536`。
界面上显示的是换算后的像素——中转站只认具体像素，发 `2K` 这种档位会被打回。
| 作品集 | 自己生成过的图，可以设成公开或删掉 |
| 系统设置 | 账号信息、改中文名、改密码、退出 |

退出登录会退回介绍页，不是直接甩到登录表单——退出的人多半只是想离开工作台。

作品集里的图由服务端存着（`generations` + `generation_images` 两张表，图片存 BLOB），
上游链接过期也不影响。**新图默认就是公开的**，想藏起来自己在作品集里点「取消公开」。

生图类型（GPT / GEMINI / GROK）和调用方式（gpt / nano / gemini-official / gemini-batch）
是两个维度，**并存**：类型是给用户挑的分组，调用方式管请求怎么发，是 Key 的属性。
一把 Key = 一个类型 + 一个调用方式，同一个类型下面可以挂不同调用方式的 Key。
用户挑完类型，服务端在那个类型下面挑一把 Key，用**那把 Key 的**调用方式发请求。

费用 = `每次生图消耗 × 类型倍率 × 模型倍率`，向上取整。向上取整是因为额度是整数：
1.5 倍的模型按 1 倍收，倍率就等于没有；向下取整又会少扣。
模型倍率在「API 管理」里点模型数量拉出来的抽屉里维护，**按模型名全局存**——
同一个模型换把 Key 来发价钱一样。不填就是 1.0x，等于这个模型不加价。

挑 Key 时会优先挑「模型列表里确实有这次要用的模型」的那把，其次是还没拉过模型列表的，
最后才轮到「拉了但没这个模型」的。中转站是按分组给模型的，配了 Key 不代表这把 Key
的账号支持这个模型——挑错了用户收到的是 `not supported by any configured account in this group`
这种看不懂的报错。

服务端**每 5 分钟**把启用中的 Key 的余额和模型各拉一遍（起来时先拉一次），
管理端那两个手动按钮也还在。不刷的话挑 Key 一直按旧数据挑：钱花完了还在挑它，
模型下架了还在发它。逐个串行拉，失败只记错误、不清掉上次拉到的值。

顶栏那个中转站余额，**管理员看真账，别人看模糊过的**：低于 10 照实显示，
高于 10 就乘一个 5-10 的随机倍数并缀上「充足」。模糊在后端做，真数字不下发——
放在前端算的话，它在 `/api/me` 的响应里躺着，翻一下 devtools 就看见了。
倍数按「用户 + 当天」定死，不是每次请求摇一次：刷新一下数字就变，看着像坏了。

菜单栏底部和用户菜单里的「管理端」**只有管理员看得见**，普通用户那边根本不渲染。
判断依据是用户表上的 `is_admin` 标志，不是「名字叫 admin」——管理员没登过用户端时，
`admin` 这个名字是能被普通用户抢注的，靠名字判断会认错人。

介绍页和登录页上**不露管理入口**，管理端只能自己敲 `/admin` 进。这只挡视线不挡人：
所有 `/api/admin/*` 都要 `darkroom_admin` 这个 Cookie 的会话，没有一律 401，
用户端的 `darkroom_user` 顶不上——两个会话是分开的。

## 后端

`web/` 是纯静态前端，只认 `/api` 和 `/health`，不知道后端是什么写的。后端只有一套：
`server-go/`，Go + SQLite / MariaDB（两种都支持，见下面「数据库」一节）。

|  |  |
| --- | --- |
| 依赖 | `modernc.org/sqlite`（纯 Go，不需要 CGO 和 gcc）、`github.com/go-sql-driver/mysql`、`golang.org/x/crypto` |
| 存储 | 默认 `data/darkroom.db`（SQLite）；设了 `DB_HOST` 就用 MariaDB |
| 默认端口 | 6670 |
| 启动 | `cd server-go && go run .`，或 `go build -o darkroom.exe . && ./darkroom.exe` |

需要 Go 1.26 或更高版本——路由本身 1.22 就够，但 `modernc.org/sqlite` 那一串依赖要求 1.26。

### 前后端分开跑

默认就是这么跑的，属于跨源。所以

- `web/config.js` 里把请求指向 `http://<当前主机名>:<后端端口>`；
- 后端必须回 CORS 头，而且因为登录态是 HttpOnly Cookie，`Access-Control-Allow-Origin`
  只能回具体来源、不能是 `*`，还要带 `Access-Control-Allow-Credentials`。
  放行名单默认是 `http://127.0.0.1:6664` 和 `http://localhost:6664`，
  用 `CORS_ORIGINS` 可以改（逗号分隔）。

想把前后端合成同源（比如直接用后端托管 `web/`），把 `web/config.js` 里的
`window.DARKROOM_API` 改成空串即可，后端会照常提供静态文件。

### 环境变量

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `6670` | 监听端口 |
| `CORS_ORIGINS` | `http://127.0.0.1:6664,http://localhost:6664` | 放行哪些来源跨源访问 |
| `DATA_FILE` | `../data/darkroom.db` | SQLite 文件路径（不设 `DB_HOST` 时才用） |
| `DB_HOST` | 无 | 设了就用 MariaDB，不设就退回 SQLite |
| `DB_PORT` | `3306` | MariaDB 端口 |
| `DB_NAME` | `image` | 库名 |
| `DB_USER` | `image` | 库账号 |
| `DB_PASSWORD` | 无 | 库密码。设了 `DB_HOST` 就必须给，否则拒绝启动 |
| `DB_TLS` | `skip-verify` | 连 MariaDB 的加密方式。默认强制加密但不校验证书（服务端多是自签证书）；`preferred` 是有就用、没有退回明文 |
| `DB_TLS_CA` | 无 | 服务端证书的 CA（PEM 文件）。设了它就能真正校验证书，这时 `DB_TLS` 会失效 |
| `WEB_DIR` | `../web` | 前端目录 |
| `ADMIN_USER` | `admin` | 管理端账号 |
| `ADMIN_PASSWORD` | 无 | 设了之后每次启动都会把管理端对齐到这个值，忘了密码时也能靠它找回 |
| `RELAY_HOSTS` | uuapi 那四个域名 | 中转域名白名单，逗号分隔。只有自建中转或本地起桩测试才需要改；放开它等于允许把请求发到任意主机 |
| `RELAY_CA_FILE` | 无 | 额外信任的 CA 证书（PEM），用来接自签证书的中转站 |

## 数据库：SQLite 还是 MariaDB

两种都支持，靠 `DB_HOST` 切。**测试全跑在 SQLite 上**——不设 `DB_HOST` 时
`go test` 用的是临时文件，不用为了跑一遍测试去连远端。建表语句是一份、两边通用
（`VARCHAR` 在 SQLite 里是 TEXT 亲和，`LONGBLOB` 是 BLOB 亲和），只有下面几处
按方言分开，都在 `store.go` 里标了注释：

| 差异 | SQLite | MariaDB |
| --- | --- | --- |
| 「已存在就跳过」 | `INSERT OR IGNORE` | `INSERT IGNORE` |
| 「有则改无则插」 | `ON CONFLICT … excluded.x` | `ON DUPLICATE KEY UPDATE … VALUES(x)` |
| 查表结构 | `PRAGMA table_info` | `information_schema.columns` |
| 邮箱唯一索引 | 条件索引 `WHERE email_lower <> ''` | 普通唯一索引（MariaDB 不支持条件索引） |
| 审计日志的插入顺序 | 隐式 `rowid` | 显式 `seq BIGINT AUTO_INCREMENT` |

**几个踩过的坑，换库前先看**：

- **`keys` 是 MariaDB 保留字**，不加反引号查不了。表已改名 `api_keys`（SQLite 那边
  也跟着改了，老库启动时自动 rename），不然得在十几条语句里到处补引号，
  而 Go 的原始字符串里还写不了反引号。
- **`max_allowed_packet` 决定单张图能不能存进去**。默认 16MB，超了这条 UPDATE 会被
  整个拒掉，而且**连接会跟着断**（实测），同一个连接上后面几条查询一起完蛋。
  所以存图那一步主动卡在「服务端的包上限 - 1MB」，超了不写库、只留 `source_url`，
  作品集退回用上游链接——图还看得见，只是链接会过期。
  服务端的值在启动时读一次（日志里会打出来），**调大服务端之后重启一下服务**即可，
  不用改代码。要执行的 SQL 见 `docs/mariadb-tuning.sql`。
- **MariaDB 允许 `TEXT` 带 `DEFAULT`，MySQL 不允许**。这份 schema 有几十个
  `TEXT NOT NULL DEFAULT ''`，迁到 MySQL 上会全挂。
- **应用层那把互斥锁只在单进程内有效**。它是给 SQLite 准备的（一个连接 + 一把锁 =
  天然全串行），换 MariaDB 后照样管用，但多开一个实例就各锁各的，扣额度那种
  读-改-写会互相踩。要真正并发起来得把那二十来个方法改成事务 + 行锁。

**搬数据**（一次性）：

```bash
cd server-go
IMAGE_DB_PASSWORD='...' go test -run TestMigrateSQLiteToMariaDB -v .
```

目标库里只要有任何一张表非空就会拒绝执行，不会覆盖已有数据。

## 上手顺序

1. 打开 http://127.0.0.1:16666/admin ，用 `admin` / `admin@123` 进入管理端。
2. 在「生图 Key」里加一把 Key：选类型，填中转地址和 API Key。GPT / 香蕉 / 批量填
   `https://uuapi.io/v1`，官方直连填 `https://uuapi.io`。保存后点「刷新余额」，确认能查到余额。
3. 回到 http://127.0.0.1:16666/login ，注册一个账号，进工作台后点「签到领额度」。
4. 选调用方式、模型，写好描述，点「生成」。

介绍页的「开始创作」会先问一次 `/api/me`：登录了就进工作台，没登录就进登录页。
工作台自身也认这一条——没会话时打开 `/studio` 会直接跳回登录页。

## 批量生图

一次提交多条提示词，走 Gemini 的批量任务合同，适合跑量。提交时按条目数扣额度
（每条 `output_count` 张算一次），上游受理之后就不再退还；提交本身失败会全额退回。

![批量生图](docs/studio-batch.png)

每个任务可以刷新状态、下载 ZIP、取消，或者删掉记录。下载和单项图片是原始字节透传，
不经过 JSON 包装。

## 管理端

![管理端](docs/admin.png)

左边一条菜单栏，几块内容分开放在 `#users` / `#keys` / `#types` / `#checkin` / `#audit`
几个 hash 下，刷新和前进后退都能回到原来那一页：

- **用户管理**：改额度、重置密码、停用 / 启用、删除。停用或重置密码会立刻踢掉该用户的所有会话。
- **API 管理**：Key 的增删改查、启用停用、刷新余额、**拉取模型**、逐把 Key 配置请求头 `User-Agent`。
  中转地址只接受 `uuapi.io`、`uuapi.net`、`uuapi.shop`、`uuapi.cc` 四个域名下的 https 地址。
- **生图类型**：每个类型的默认模型和倍率，以及逐个模型的倍率。
- **签到**：每天签到领多少额度（填个区间，比如 2-5，签到时随机给）、每次生图扣多少额度，
  以及改管理端自己的密码。
- **审计日志**：谁在什么时候做了什么，见下。

### 审计日志

`audit_logs` 表，**只增不改不删**。记的是登录（含登录失败）、登出、注册、签到、生图、
公开/取消公开、删除作品，以及管理端所有改配置、改用户、改 Key 的动作。

- 每条都带操作者、对象、详情、来访 IP 和时间。管理端和用户端的操作分开标，
  因为它们是两套会话，混在一起看不出是谁干的。
- **不记**查余额、拉模型列表这类纯读操作——后台每 5 分钟自动跑一遍，记下来只会把日志淹掉。
- **Key 的密钥不进审计**：审计是给人翻的，不该变成第二个泄露面。
- 写审计失败只在服务端日志里留痕，绝不把已经成功的签到、生图搅黄——它是旁路。
- 翻页按 `rowid` 倒序，不按时间戳：时间戳只到秒，同一秒里连着发生的几件事按时间排会乱套，
  而审计要看的恰恰是先后。

### 余额是怎么查的

「刷新余额」只打一个接口：`GET {中转地址去掉 /v1}/v1/usage`，带 `Authorization: Bearer <API Key>`。
响应按下面这套规则解析，和把中转站导入 cc-switch 时用的 extractor 完全一致：

| 字段 | 取值 |
| --- | --- |
| 余额 | `remaining` → `quota.remaining` → `balance` |
| 单位 | `unit` → `quota.unit` → `USD` |
| 是否有效 | `is_active` → `isValid` → 默认有效 |

只有中转站返回了明确的布尔值才算数。一旦读到无效，这把 Key 会标红显示「Key 已失效」，
并且不再被挑去生图。

手机上是这样：

<img src="docs/studio-mobile.png" alt="手机端" width="320" />

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/auth/register` | 注册并登录 |
| `POST` | `/api/auth/login` | 登录 |
| `POST` | `/api/auth/logout` | 退出 |
| `GET` | `/api/me` | 当前用户、签到额度区间、每次生图消耗 |
| `POST` | `/api/checkin` | 签到领额度，返回这次随机到的 `amount` |
| `GET` | `/api/checkins` | 自己的签到记录，倒序分页 |
| `GET` | `/api/models` | 各调用方式下中转站实际支持的模型 |
| `POST` | `/api/me/password` | 改自己的密码 |
| `PATCH` | `/api/me` | 改中文名。body `{displayName}`，最多 24 字 |
| `POST` | `/api/generate` | 生图（先扣额度，失败退回）。中转站的报错会补一句中文提示 |
| `GET` | `/api/generations` | 自己的作品集。`limit`（默认 24，上限 100）、`offset` |
| `GET` | `/api/generations/{id}` | 作品详情。自己的，或已公开的 |
| `PATCH` | `/api/generations/{id}` | 公开 / 取消公开。body `{isPublic}` |
| `DELETE` | `/api/generations/{id}` | 删除自己的作品 |
| `GET` | `/api/generations/{id}/images/{position}` | 作品图片（原始字节） |
| `GET` | `/api/works` | 公开作品。不登录也能看 |
| `POST` | `/api/batches` | 提交批量任务（按条目扣额度，提交失败退回） |
| `GET` | `/api/batches` | 批量任务列表 |
| `GET` | `/api/batches/models` | 批量可用的模型 |
| `GET` | `/api/batches/{id}` | 任务详情 |
| `GET` | `/api/batches/{id}/items` | 任务条目 |
| `GET` | `/api/batches/{id}/items/{custom_id}/content` | 单项内容（原始字节） |
| `GET` | `/api/batches/{id}/download` | 下载 ZIP（原始字节） |
| `POST` | `/api/batches/{id}/cancel` | 取消任务 |
| `DELETE` | `/api/batches/{id}/outputs` | 删除输出 |
| `DELETE` | `/api/batches/{id}` | 删除任务记录 |
| `POST` | `/api/admin/login` | 管理端登录 |
| `GET` | `/api/admin/state` | 签到规则、Key 列表、用户列表 |
| `PUT` | `/api/admin/settings` | 改签到规则 |
| `POST` | `/api/admin/password` | 改管理密码 |
| `POST` | `/api/admin/keys` | 新增 / 修改 Key |
| `DELETE` | `/api/admin/keys/{id}` | 删除 Key |
| `POST` | `/api/admin/keys/{id}/balance` | 向中转站查询并记录余额 |
| `POST` | `/api/admin/keys/{id}/models` | 向中转站查询这把 Key 能用的模型 |
| `PATCH` | `/api/admin/users/{id}` | 改用户额度，或停用 / 启用（`quota`、`disabled`） |
| `POST` | `/api/admin/users/{id}/password` | 重置某个用户的密码 |
| `DELETE` | `/api/admin/users/{id}` | 删除用户 |
| `GET` | `/health` | 健康检查 |

`POST /api/auth/register` 的正文：

```json
{
  "username": "wuyi2026",
  "displayName": "吴一",
  "password": "abcd1234",
  "phone": "13800138000",
  "email": "wuyi@example.com"
}
```

| 字段 | 规则 |
| --- | --- |
| `username` | 必填。2–20 位，**只能用英文字母和数字**，不区分大小写地唯一 |
| `displayName` | 可选。中文名，最多 24 个字 |
| `password` | 必填。**至少 8 位**，最多 72 位 |
| `phone` | 必填。11 位大陆手机号（1 开头、第二位 3–9），不查重 |
| `email` | 必填。格式校验，不区分大小写地唯一，**可以当登录名用** |

`POST /api/auth/login` 的正文：

```json
{ "account": "wuyi2026", "password": "abcd1234" }
```

`account` 既收名称也收邮箱，两者都不区分大小写。

**管理端的账号密码也能登用户端。** 用户表里查不到（或密码对不上）时，会再拿 `admin` 表比一次；
对上了就给它补一条用户记录，之后走普通用户那一套——额度、签到、生图都正常。
补出来的记录邮箱手机号留空，所以走不了邮箱登录，也不占用唯一邮箱。

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
要局部重绘就再加上 `mask` / `maskUrl`（同样是一份文件或一个 https 地址）。

给网址的那一半由**服务端先取回来**：验状态码、验字节大小、认魔数确认真是图片，
然后一律按 multipart 发给上游。上游不用自己取图，热链、防盗链、签名短链过期
这些坑就都跟这次请求无关了。取不到或者不是图片会当场报 400，说清楚是参考图还是蒙版。
认魔数不认响应头——对象存储经常把真图的 content-type 标成 `application/octet-stream`。

返回：

```json
{ "ok": true, "channel": "async", "taskId": "task_8f21c4", "images": [{ "url": "https://..." }], "quota": 41 }
```

`images` 里的元素要么是 `{ url }`，要么是 `{ b64, mime }`。`channel` 是 `async` / `sync` /
`chat` / `gemini`，表示这次实际走的是哪条链路。

`POST /api/batches` 的正文：

```json
{
  "model": "gemini-2.5-flash-image",
  "provider": "gemini_api",
  "image_size": "1K",
  "response_mime_type": "image/png",
  "items": [
    { "custom_id": "cover_001", "prompt": "A clean product hero image", "output_count": 1 }
  ]
}
```

单个任务最多 200 个输出，每个条目最多 4 张，`custom_id` 只能用字母、数字、下划线、点和短横线。
额度按所有条目的 `output_count` 之和扣。

## 目录

前后端是分开的两块，中间只有 `/api` 这一层约定：

```
web-server.js       前端静态服务（只发 web/，不碰数据）
web/                前端（纯静态，没有构建步骤）
  index.html          介绍页（首页，飞天 hero）
  login.html          登录 / 注册
  studio.html         工作台首页（额度、签到）
  create.html         创作（生图表单、批量）
  gallery.html        作品集（自己的）
  works.html          公开作品
  settings.html       系统设置
  admin.html          管理端
  styles.css          设计体系
  landing.js          介绍页的登录态分流
  login.js            登录 / 注册逻辑
  shell.js            工作台外壳：左菜单栏、顶栏用户菜单、登录态
  home.js             首页
  create.js           创作
  gallery.js          作品集 / 公开作品（两页共用）
  settings.js         系统设置
  admin.js            管理端
  config.js           后端地址
  art/                敦煌图（介绍页 hero、登录页壁画）
server-go/          后端（Go + SQLite）
  main.go             HTTP 服务、路由、参数校验
  store.go            建表与用户、会话、Key、额度的读写
  relay.go            上游调用与降级
docs/
  API.md              接口契约
  *.png               README 里的截图
data/               运行时数据（已 gitignore）
```

前端只依赖 `/api` 和 `/health`，不知道后端是什么写的。想换一套后端也行：照着 `docs/API.md`
实现接口即可，`web/` 一个字节都不用动；也可以把 `web/` 丢给任何静态服务器，
再用 `web/config.js` 把请求指到别的地址。

## 说明

- 数据都写在 `data/darkroom.db` 里，API Key 是明文存的。别把这个目录提交上去，也别把服务开到公网。
- 会话用 HttpOnly Cookie，有效期 14 天。管理端和用户端的会话是分开的。
- 请求上游超时 120 秒，异步任务最多轮询 180 秒；请求体上限 96MB，参考图和蒙版上限 64MB。
- 中转地址和参考图地址都只收 https，参考图地址还会挡掉本机、内网和 `.local` / `.internal`，
  跟随重定向最多 3 跳。
- README 里的截图是对着一个假的中转站跑出来的：画面是画出来的占位图，批量任务也是桩数据。
  界面本身和请求流程都是实际的。
