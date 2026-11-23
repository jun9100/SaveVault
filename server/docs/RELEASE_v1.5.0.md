# game-save v1.5.0（SaveVault 云端）

镜像：`jun9100/game-save:v1.5.0`（`linux/amd64`、`linux/arm64`）。基于 `v1.4.1`，数据目录格式不变，可以直接升级，也可以回滚到 `v1.4.1`。

## 变更

- **完整性校验覆盖 SaveVault 存档。** 维护页的“完整性校验”原来只检查 Eden 的加密对象。现在也会逐个重算 SaveVault（SaveVault）存档的 SHA-256，并与服务端收下时的摘要比对。存档丢失、损坏或无法读取时，会报告出类型、范围和文件名。
- **运维状态**：仪表盘新增健康、存储、保留策略和最近同步信号；Bark 通知失败不再影响上传（P4）。
- **改名**：管理网页显示为“SaveVault 云端”，SaveVault 存档页的文案也一并改名；换了新的盾牌图标。
- **文档**：P5 隔离回归清单、服务端回归脚本 `scripts/p5_service_rows.py`、P5 结果记录、云端优化计划。

接口、环境变量、`/v1/savevault` 路径和客户端依赖的错误信息都没有变，现有客户端不需要任何修改。

## 升级（NAS / 群晖）

1. **记下当前状态**：确认 `GET /healthz` 返回 `ok`；在管理网页的概览页记下各存档范围的数量。
2. **停掉旧容器**：`docker compose stop game-save`，或在 Container Manager 里停止。不要让两个容器同时使用同一个数据目录。
3. **完整备份数据目录**：例如 `cp -a /volume1/docker/game-save/data /volume1/docker/game-save/data-backup-<日期>`。
4. **改镜像标签**：把 compose 里的镜像改成 `jun9100/game-save:v1.5.0`，然后 `docker compose pull && docker compose up -d`。
5. **验证**：
   - `GET /healthz` 返回 `ok`，管理网页标题显示“SaveVault 云端”，版本号为 `v1.5.0`；
   - 概览中的范围数量和升级前一致；
   - 在维护页运行一次“完整性校验”，结果应为“全部正常”。

## 回滚

1. 停止 `v1.5.0` 容器。
2. 把镜像标签改回 `jun9100/game-save:v1.4.1`。如果升级后已经有新的上传，又想完全回到升级前，就用第 3 步的备份替换数据目录；否则直接沿用当前数据目录即可（格式没有变）。
3. 启动后按上面第 5 步验证。
