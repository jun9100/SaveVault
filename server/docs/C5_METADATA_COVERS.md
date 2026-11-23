# C5：元数据与封面服务

[`SAVEVAULT_CLOUD_PLAN.md`](SAVEVAULT_CLOUD_PLAN.md) 的 C5 阶段，服务端方案。本文只是设计，
其中的接口是 Android、PC 两端要依赖的约定。2026-11-08 已确认，决定见文末“已确认的决定”。

## 现状

| | 名称 | 封面 | 自定义封面 |
| --- | --- | --- | --- |
| Android | 内置约 2.4 万条 Switch 名称表（`switch-titles.tsv`，来自 titledb，含 eShop 图标哈希）；Steam 等靠本机识别 | 从 Steam CDN、Nintendo eShop CDN、tinfoil.media 下载，缓存在本机 | 用户挑一张图，只存在本机 |
| PC | 没有 Switch 名称表 | 本机守护进程依次尝试 4 个 Steam CDN 地址，失败再走代理，缓存在本机 | 一个图片 URL，只存在本机 |
| Eden | 范围元数据里的标题和图标（`PUT /v1/scopes/{id}/metadata`） | 同左，客户端上传 | — |

问题：

- 国内网络下 Steam CDN 和 tinfoil 经常超时，每台设备都要各自重试；
- 同一款游戏在手机和 PC 上名字不同，PC 上的 Switch 游戏常常只显示 TitleID；
- 自定义封面换一台设备就没了。

## 目标与不做的事

目标：

1. **缓存封面**：服务端从固定的几个上游取一次封面，存在 NAS 上，所有设备从服务端读；
2. **统一名称表**：服务端给每个 C1 游戏算出一个显示名，各端显示同一个名字；PC 也能拿到 Switch 名称表；
3. **自定义名称和封面跨设备同步**：在一台设备上改了，其他设备都能看到。

不做的事：

- **不按客户端给的 URL 抓图**。服务端只访问写死的上游地址模板，参数全部按格式校验，杜绝 SSRF；
- 不改 Eden。Eden 的范围 ID 是哈希，服务端不知道它对应哪个 TitleID；Eden 的标题和图标仍然按现状存在 `dashboard.json`；
- 不同步 Android 应用图标、PSP 存档里的 `ICON0.PNG`。前者每台设备本机都有，后者本来就在存档归档里；
- 不做竖版封面、背景图等多种规格。第一版每个游戏一张封面；
- 不碰存档：元数据和归档、CAS 头、冲突副本完全分开，任何元数据操作都不会让头前进。

## 元数据挂在哪里

元数据挂在 **C1 的游戏键**上，比如 `switch/title:0100000000010000`、`pc/steam:1245620`、`pc/scope:elden-ring`。

- 客户端不需要知道游戏键怎么算：写接口按范围（`{type}/{game}`）寻址，服务端在写入时算出这个范围当前的游戏键；
- 读的时候，`/v1/opensave/games` 里每个游戏直接带上元数据；
- 网页上手动关联范围、改变了它的游戏键之后，这个范围就显示新游戏键的元数据。原来那个键的元数据保留，以后关联回去还在。

## 名称

### 解析顺序

服务端按下面的顺序给每个游戏算显示名，取第一个非空的：

| 顺序 | 来源 | `nameSource` |
| --- | --- | --- |
| 1 | 自定义名称（客户端或网页设置，跨设备同步） | `custom` |
| 2 | 服务端名称表（目前只有 Switch，按 TitleID 查） | `table` |
| 3 | 成员范围上报的身份名称（C1 `identity.name`），取最近更新的那个 | `client` |
| 4 | 游戏键里的 ID 部分 | `key` |

自定义名称最长 120 个字符（和 C1 的身份名称相同），不能含控制字符；设为空字符串表示清除。

### Switch 名称表

- 与 Android 内置的同源同格式：每行 `TitleID<TAB>名称<TAB>eShop 图标哈希`，优先中文名；
- 用同一个脚本生成，以 gzip 形式通过 `go:embed` 编进二进制（约 1.6 MB），每次发版时更新。服务端不在运行时联网更新名称表；
- 数据来自 [titledb](https://github.com/blawar/titledb)，许可状态不明确。本项目仅供自托管使用（Android 已经内置同一份数据）；
- 表有版本号，取生成时上游数据的日期，比如 `2026-11-01`；
- 客户端可以下载整张表（见下面的接口），PC 用它补上 Switch 名称，Android 可以用它更新内置表而不必等发新版 APK。

## 封面

### 解析顺序

| 顺序 | 来源 | `cover.source` |
| --- | --- | --- |
| 1 | 自定义封面（客户端或网页上传，跨设备同步） | `custom` |
| 2 | 服务端从上游缓存的封面 | `cached` |
| 3 | 没有 | 不返回 `cover` |

服务端没有封面时，客户端照旧用本机的方式取图。

### 封面文件

- 所有封面（自定义的和缓存的）都按内容的 SHA-256 存放：`opensave-meta/covers/<前两位>/<sha256>`。内容相同只存一份；
- 只接受 JPEG 和 PNG：先检查文件头，再用 `image.DecodeConfig` 读尺寸；
- 自定义封面最大 1 MiB、最长边不超过 2048 像素；上游封面最大 2 MiB。客户端上传前应先缩小（Android 现在已经会缩小）；
- 服务端不改图片内容，也不转码。

### 从上游缓存

只访问下面这些固定模板，`{…}` 里的参数都先按格式校验：

| 游戏键 | 上游，按顺序尝试 |
| --- | --- |
| `*/steam:{AppID}`（1–12 位数字） | `https://cdn.cloudflare.steamstatic.com/steam/apps/{AppID}/header.jpg`，以及 PC 端已在用的另外 3 个 Steam CDN 地址 |
| `switch/title:{TitleID}`（16 位十六进制） | 名称表里有图标哈希时：`https://img-eshop.cdn.nintendo.net/i/{哈希}.jpg`；然后 `https://tinfoil.media/ti/{TitleID}/0/0/` |
| 其他 | 不抓取 |

规则：

- **默认关闭**。由管理员在网页上打开（或用环境变量 `GAME_SAVE_COVER_FETCH=1`）。打开后服务端会以 NAS 的身份访问 Steam、Nintendo 和 tinfoil，并把 AppID、TitleID 带给它们；
- 只在写入路径和后台任务里抓取：上报身份（`PUT .../identity`）之后排队，维护任务定期补抓。**只读接口不会触发抓取**，和“读取不写盘”的规则一致；
- 并发最多 2 个，单次请求超时 8 秒；
- 重定向最多 3 次，而且只能跳到上表里的主机；
- 使用标准的 `HTTPS_PROXY` 环境变量。国内网络下 Steam CDN 和 tinfoil 常常超时，建议给容器配上 `HTTPS_PROXY`（必要时加 `NO_PROXY` 排除局域网地址）；
- 上游返回 404：记为“没有封面”，24 小时内不再试；网络错误：按 1 小时、6 小时、24 小时退避；
- 网页上可以对某个游戏“重新抓取”，清掉这些记录立即重试。

### 自定义封面同步

- 客户端上传图片字节，不上传 URL。PC 端现在的自定义封面是一个 URL，需要先在本机下载，再把图片上传；
- 写入带乐观并发控制：每个游戏的元数据有一个修订号 `rev`，每次修改加一。写请求带 `If-Match: "<rev>"`，与当前不符时返回 412。这样一台设备离线时排队的旧修改，不会覆盖另一台设备后来的选择；
- 收到 412 时，客户端重新读取，把服务端的结果显示出来。用户如果仍然想用自己的那张，再发一次；
- 清除自定义封面后，回到缓存的上游封面。

## 接口

字段名和 C1 的游戏接口一样用 camelCase。所有写接口都记录设备心跳；内容不变时不写盘，返回 204。

### 客户端接口

| 方法 | 路径 | 认证 | 作用 |
| --- | --- | --- | --- |
| `GET` | `/v1/opensave/games`、`/v1/opensave/games/{key}` | 同步令牌（只读） | 已有接口，每个游戏新增 `meta` 字段 |
| `GET` | `/v1/opensave/covers/{sha256}` | 同步令牌（只读） | 下载封面 |
| `PUT` | `/v1/opensave/scopes/{type}/{game}/custom-name` | 同步令牌 | `{"name": "…"}`，设置或清除（空字符串）自定义名称 |
| `PUT` | `/v1/opensave/scopes/{type}/{game}/custom-cover` | 同步令牌 | 请求体是图片本身，`Content-Type: image/jpeg` 或 `image/png` |
| `DELETE` | `/v1/opensave/scopes/{type}/{game}/custom-cover` | 同步令牌 | 清除自定义封面 |
| `GET` | `/v1/opensave/titles/switch` | 同步令牌（只读） | 下载 Switch 名称表 |

**`meta` 字段**

```json
{
  "key": "switch/title:0100000000010000",
  "name": "超级玛利欧 奥德赛",
  "meta": {
    "rev": 3,
    "name": "超级玛利欧 奥德赛",
    "nameSource": "table",
    "cover": {
      "source": "custom",
      "sha256": "9f2c…",
      "mediaType": "image/jpeg",
      "width": 460,
      "height": 215,
      "url": "/v1/opensave/covers/9f2c…"
    },
    "updatedAt": "2026-11-08T12:00:00Z",
    "updatedBy": "a1b2c3d4-…"
  }
}
```

- 游戏顶层的 `name` 继续保留现在的含义，`meta.name` 是解析后的显示名。新客户端用 `meta.name`；
- 没有任何元数据记录的游戏，`meta` 里只有 `rev: 0` 和名称；没有封面时没有 `cover`；
- `updatedBy` 是设备 ID，网页修改时为 `admin`。设备 ID 的取法与 C3 一致：用设备令牌访问时，取令牌绑定的设备 ID，忽略请求头里的设备 ID（`X-Eden-Device-ID` / `X-OpenSave-Device-ID`）；用共享令牌访问时，沿用请求头。

**写接口的返回**

- 成功：200，返回这个游戏最新的 `meta`；
- `If-Match` 不符：412，同样返回最新的 `meta`，客户端不必再读一次；
- 不带 `If-Match`：直接覆盖。只建议用于用户刚刚明确操作、而且客户端没有缓存修订号的情况；
- 范围不存在：404；图片格式、尺寸、大小不对：400 或 413。

**`GET /v1/opensave/covers/{sha256}`**

- 内容按哈希寻址、不会变，响应带 `Cache-Control: private, max-age=31536000, immutable` 和 `ETag`；
- 客户端按 sha256 缓存在本机，哈希变了才重新下载。

**`GET /v1/opensave/titles/switch`**

- 响应是 gzip 压缩的 TSV（`Content-Type: application/gzip`），格式同 Android 内置表；
- `ETag` 是名称表版本号。客户端带 `If-None-Match` 时，没变就返回 304。

### 管理接口

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| `PUT` | `/admin/api/opensave/games/{key}/custom-name` | 同客户端接口，按游戏键寻址 |
| `PUT`、`DELETE` | `/admin/api/opensave/games/{key}/custom-cover` | 同上 |
| `POST` | `/admin/api/opensave/games/{key}/cover/refetch` | 清掉“没有封面”和退避记录，立即重新抓取 |
| `GET` | `/admin/api/opensave/covers/{sha256}` | 网页显示封面用 |
| `GET`、`PUT` | `/admin/api/meta-settings` | `{"coverFetch": true}`，上游抓取开关 |

`/admin/api/opensave/games` 同样带上 `meta`。网页部分（游戏列表显示封面、修改名称和封面、抓取开关）等管理网页改版合入后再做。

## 存储

新文件 `opensave-meta.json` 和新目录 `opensave-meta/`，都只在第一次需要时创建：

```json
{
  "version": 1,
  "settings": { "coverFetch": false },
  "games": {
    "switch/title:0100000000010000": {
      "rev": 3,
      "customName": "",
      "customCover": { "sha256": "…", "mediaType": "image/jpeg", "width": 460, "height": 215 },
      "upstream": { "sha256": "…", "mediaType": "image/jpeg", "width": 460, "height": 215, "fetchedAt": "…" },
      "upstreamMissUntil": "…",
      "upstreamFailures": 0,
      "updatedAt": "…",
      "updatedBy": "…"
    }
  }
}
```

- 名称表编在二进制里，不落盘；
- 和其他状态一样，先复制、写盘成功再替换；`clone()` 要复制 `games` 这个 map；
- 维护任务删除没有任何记录引用、且存在超过 1 小时的封面文件；
- 游戏的所有范围都被删掉后，元数据记录仍然保留（很小）。网页上可以手动清理。

## 升级与回滚

### 从 v1.7.0 升级

- 不需要数据迁移，现有文件一个都不改；
- 封面抓取默认关闭，升级后服务端不会主动访问任何外部地址；
- 旧客户端不认识 `meta` 字段，会忽略它。

### 回滚到 v1.7.0

- v1.7.0 不认识 `opensave-meta.json` 和 `opensave-meta/`，会忽略它们；
- 游戏接口不再返回 `meta`，新的写接口返回 404。客户端回到本机的名称和封面，所以**客户端必须一直保留本机的封面缓存和自定义封面**，不能因为服务端有了就删掉本机的；
- 重新升级后，元数据原样恢复；
- 再往前回滚到 v1.6.0 时，C5 部分同上；另外 C3 的回滚注意事项仍然适用：用设备令牌的客户端会全部 401，需要改回共享令牌；已经切到 `deny` 的也会重新接受共享令牌，如有泄露风险先更换 `GAME_SAVE_TOKEN`。详见 [`C3_DEVICE_TOKENS.md`](C3_DEVICE_TOKENS.md) 的“回滚到 v1.6.0”。

### 备份

`opensave-meta/` 里缓存的封面可以重新抓取，但自定义封面只有这一份。备份时把整个数据目录一起备份。

## 对客户端的要求

### 共同要求

1. 旧服务端兼容：游戏没有 `meta` 字段，或新接口返回 404 时，完全按现在的方式工作；
2. 显示名：有 `meta.name` 就用它，否则用本机的；
3. 封面：有 `meta.cover` 就按 `sha256` 从服务端取，按哈希缓存在本机；没有就用本机现有的方式；
4. 自定义名称和封面：用户修改时先在本机生效，再带 `If-Match` 上传；离线时排队，联网后上传；收到 412 时以服务端的结果为准，并提示用户；
5. 本机的封面缓存和自定义封面一直保留，作为服务端不可用时的回退。

### Android

- 现在的自定义封面按本机的游戏键保存；上传时按这个游戏的任意一个范围寻址即可，服务端会算出游戏键；
- 封面缩小到最长边 1024 像素、JPEG 质量 85 左右，一般在 200 KB 以内；
- 可以选择用 `GET /v1/opensave/titles/switch` 定期更新内置的 Switch 名称表。

### PC

- 用 `GET /v1/opensave/titles/switch` 给 Switch 游戏补上名称；
- 自定义封面现在是 URL：用户设置时由本机下载图片、再上传字节；
- 服务端已有封面时，优先用服务端的，不再逐个尝试 Steam CDN。

### Eden

无要求。

## 服务端实现要点

- 新文件 `opensave_meta.go`：元数据记录、名称解析、封面文件存取、`clone()`、加载和持久化；
- 新文件 `cover_fetch.go`：上游模板、参数校验、限定主机的 HTTP 客户端、抓取队列、退避；
- `opensave_games.go` 的游戏视图里加上 `meta`；
- 名称表：脚本生成 `switch-titles.tsv.gz`，`go:embed` 进二进制，启动时懒加载；
- 指标：`game_save_covers{source="custom|cached"}`、`game_save_cover_fetch_total{result="ok|miss|error"}`。

必须覆盖的测试：

| 场景 | 预期 |
| --- | --- |
| 名称四级优先级 | 自定义 > 名称表 > 客户端上报 > 键 |
| 上传自定义封面后，另一台设备读取游戏列表 | `meta.cover.source` 为 `custom`，`sha256` 一致，能下载到同样的字节 |
| 带过期的 `If-Match` 修改 | 412，返回最新 `meta`，数据不变 |
| 上传非 JPEG/PNG、超过 1 MiB、超过 2048 像素 | 400 或 413，不留下封面文件 |
| 清除自定义封面 | 回到 `cached`，没有缓存时不返回 `cover` |
| 抓取关闭时上报身份 | 不发出任何外部请求 |
| 抓取打开，用测试上游返回 404 | 记为没有封面，24 小时内不再请求 |
| 上游重定向到不在白名单的主机 | 放弃，不跟随 |
| 只读接口（游戏列表、封面下载、名称表） | 不触发抓取，不写盘 |
| 两个游戏用同一张图 | 只存一份文件；删掉其中一个的引用后文件仍在 |
| 维护任务 | 删除无引用的封面文件，保留有引用的 |
| 任何元数据写入 | 不改变任何范围的头、归档和冲突副本 |
| 名称表下载带 `If-None-Match` | 版本没变返回 304 |
| 没用过 C5 功能 | 数据目录里不出现 `opensave-meta.json` 和 `opensave-meta/` |

测试里的上游一律用 `httptest` 假服务，不访问真实的 Steam 或 Nintendo。

## 交付顺序

1. 本文确认；
2. 服务端：名称解析和名称表、自定义名称和封面同步、`meta` 字段（不需要外网，可以先做）；
3. 服务端：上游封面缓存（默认关闭）；
4. 管理网页（排在 C3 网页之后）；
5. Android、PC 接入。

## 已确认的决定

2026-11-08 由用户确认（“按建议确认”）：

1. 上游封面抓取**默认关闭**，由管理员在网页上或用 `GAME_SAVE_COVER_FETCH=1` 打开；国内网络可以配 `HTTPS_PROXY`；
2. Switch 名称表用 `go:embed` 编进二进制，每次发版时更新；数据来自 titledb，许可状态不明确，本项目仅供自托管使用；
3. 自定义名称和封面挂在游戏键上，手动关联会让范围换用另一个游戏键的元数据，按本文执行；
4. 自定义封面采用“带修订号、后写入者胜”，不保留历史版本。
