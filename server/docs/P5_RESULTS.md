# P5 结果记录

按 [`ISOLATED_REGRESSION_MATRIX.md`](ISOLATED_REGRESSION_MATRIX.md) 执行。所有行只用一次性数据目录和随机
令牌，没有挂载 NAS 数据目录或任何真实存档。

## 服务端（2026-11-08）

- 版本：`codex/verify-savevault-archives`（`ee3eafb`，`v1.4.1-dev`），macOS 本机进程。
- 方法：`python3 -I scripts/p5_service_rows.py <binary> <临时目录>`。

| 行 | 结果 | 证据摘要 |
| --- | --- | --- |
| GS-01 | 通过 | 两次健康检查都返回 200；`switch/010000000000BEEF`、`pc/regression-pc` 在重启前后都是 `head: null`；设备列表为空。 |
| GS-02 | 通过 | 上传和发布 `snap-a` 都返回 201；重启后头仍是 `snap-a`，摘要不变。 |
| GS-03 | 通过 | `snap-b` 以 `snap-a` 为父发布成功；过期的 `snap-c` 被拒（409），之后保留为冲突，执行清理后仍在。 |
| GS-04 | 通过 | Bark 端点返回 502（共尝试 1 次），`emulator-ppsspp/regression-ppsspp` 仍发布成功，头可读。 |
| GS-05 | 通过 | 复制完整数据目录后，新进程读到的头是 `snap-b`，冲突 `snap-c` 和管理概览都在。 |
| GS-06 | 通过 | 只篡改副本中的 `pc/regression-pc__main__snap-a.zip`；校验检查 3 个对象，只报告这一个文件 `corrupt`；指标中有 `game_save_savevault_*` 和磁盘指标。 |

GS-06 在修复前未通过：完整性校验只检查 Eden 对象，篡改 SaveVault 归档后报告 0 个对象、无问题。
同一分支已修复，并补了 `TestVerifyDetectsCorruptAndMissingSaveVaultArchives`。

## Android（非 Root，2026-11-08）

- 设备：华为 MAR-AL00，Android 10，无 Root。
- 客户端：SaveVault `main`（`a306947`）的 `deviceTest` 构建，包名 `com.jun9100.savevault.test`，和正式版互不影响。
- 服务端：game-save `main`（`v1.6.0`），本机进程，一次性数据目录，随机令牌，`adb reverse tcp:18765`。
- 授权：通过系统选择器只授权 `/sdcard/SaveVault-Regression`，没有授权其他目录。
- 测完已卸载测试版，并移除端口转发、停止服务。

| 行 | 结果 | 证据摘要 |
| --- | --- | --- |
| AND-01 | 通过 | 来源显示“SAF 已授权 · 可读写 · 2 款游戏 · 3 个存档位置”；强制停止后重新打开，授权和扫描结果都还在。Android 系统选择器授予的目录权限总是可读写，“只读时只能备份”这一半在真机上无法构造，记为不适用。 |
| AND-02 | 通过 | 把夹具临时改名后重新扫描：来源显示“不可访问 · 需要 SAF 授权”并附原因，游戏库没有把它当成 0 个存档的来源；改回原名、重新扫描后，自动恢复为“SAF 已授权 · 可读写”。 |
| AND-05 | 通过 | 点“备份所选”后立即切到后台，回来时两款都显示“已同步”。共享存储里除夹具外没有新文件，夹具文件的大小和修改时间都没变。停掉服务后再备份，本地仍新建版本，状态显示“未上传”。开启 Wi-Fi 自动备份后，后台任务记录“自动备份失败，稍后重试”，并按 30 秒退避重新排队；恢复服务后，重试自动上传了 PSP 的第 2 个版本，父版本正确，没有产生冲突。 |
| AND-06 | 通过 | 按“Switch 游戏”筛选只显示 Switch 夹具，按“PPSSPP”筛选只显示 PSP 的两项。两项一起批量备份后，Switch 存档上传到 `switch`，PSP 存档上传到 `emulator-ppsspp`，没有出现 `pc`。另外，游戏身份上报（C1）在真机上生效：云端分组为 `switch/title:010000000000BEEF` 和 `emulator-ppsspp/psp:REGR00001`。 |

AND-03、AND-04 需要 Root，这台测试机做不了，记为跳过。

发现两个界面小问题，不影响结果：
- 来源不可访问时，原因直接显示系统的英文报错（“The selected URI is not a directory”）。
- 唯一的授权目录不可访问时，游戏库提示“还没有可读取的来源，请添加…”，没有指出是已有授权失效。

手动批量备份上传失败后不会自动补传，要等 Wi-Fi 自动备份或下次手动备份。

## Windows

待执行：WIN-01…06。需要一台 Windows 虚拟机，并使用独立的数据目录。
