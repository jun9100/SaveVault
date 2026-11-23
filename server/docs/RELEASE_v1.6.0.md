# game-save v1.6.0（SaveVault 云端）

镜像：`jun9100/game-save:v1.6.0`（`linux/amd64`、`linux/arm64`）。基于 `v1.5.0`，可以直接升级，也可以回滚到 `v1.5.0`。

## 变更

- **SaveVault 存档按游戏分组（C1）。** 同一款游戏在不同设备上的存档范围，在管理网页里合并显示成一个游戏，下面列出各个范围。
  - 分组依据：Switch TitleID、Steam AppID、PSP 游戏编号、安卓包名。这些信息由客户端在备份成功后上报。
  - 认不出来的范围，可以在范围页的“所属游戏”卡片里手动关联到同类型的游戏，也可以取消关联。
  - 分组只影响显示：不改动任何版本，不跨存档类型合并，也不会自动跨设备恢复。
  - 新接口：
    - `PUT /v1/savevault/scopes/{type}/{game}/identity`（客户端上报游戏信息）
    - `GET /v1/savevault/games`
    - `GET /v1/savevault/games/{key}`
    - 管理端 `GET /admin/api/savevault/games`
    - 管理端 `PUT /admin/api/savevault/scopes/{type}/{game}/game`
- **网页下载 SaveVault 版本（C4）。** 范围页的每个版本（包括冲突副本）和存档审计里的每个存档都能直接下载成 ZIP。只有登录管理员才能下载，同步 token 不能下载。

已有接口、环境变量和错误信息都没有变。旧客户端照常工作，只是不上报游戏信息；SaveVault 2.5.0 之后的客户端遇到旧服务端，会自动跳过上报。

## 数据格式

范围条目里新增两个可选字段：`savevault-native-state.json` 中的 `identity` 和 `gameLink`。`v1.5.0` 读取时会忽略这两个字段，所以可以随时回滚。

## 升级（NAS / 群晖）

1. **记下当前状态**：确认 `GET /healthz` 返回 `ok`；在管理网页的概览页记下各存档范围的数量。
2. **停掉旧容器**：`docker compose stop game-save`，或在 Container Manager 里停止。不要让两个容器同时使用同一个数据目录。
3. **完整备份数据目录**：例如 `cp -a /volume1/docker/game-save/data /volume1/docker/game-save/data-backup-<日期>`。
4. **改镜像标签**：把 compose 里的镜像改成 `jun9100/game-save:v1.6.0`，然后 `docker compose pull && docker compose up -d`。
5. **验证**：
   - `GET /healthz` 返回 `ok`，版本号为 `v1.6.0`；
   - 概览中的范围数量和升级前一致；
   - SaveVault 页面按游戏列出存档，范围页的版本行有下载按钮；
   - 在维护页运行一次“完整性校验”，结果应为“全部正常”。

## 回滚

1. 停止 `v1.6.0` 容器。
2. 把镜像标签改回 `jun9100/game-save:v1.5.0` 后启动，直接沿用当前数据目录即可。
3. 回滚后不再显示游戏分组。`v1.5.0` 不认识新字段，只要它写过一次状态文件（比如有新的上传），这些字段就会丢失。再次升级后，客户端下次备份时会重新上报游戏信息，自动分组随之恢复；手动关联则需要重新设置。
