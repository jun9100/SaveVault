// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package com.jun9100.savevault

import android.content.Context
import androidx.work.WorkManager
import com.jun9100.savevault.batch.BatchWorker
import java.io.File

/**
 * Everything the client remembers about "the cloud" is really an account's
 * history: the last head it published, the verdicts of a check, the scopes the
 * server deleted, pending retries, cached remote heads, and uploaded flags on
 * local snapshots. Switching to another account (a tenant login, a different
 * server, a new token) makes all of it meaningless, and a stale head or upload
 * flag is worse than none: backups skip uploading or refuse to publish because
 * they believe the old vault's history still applies.
 *
 * Clearing all cloud-bound local state on an account change keeps a fresh
 * account usable from the first scan.
 */
internal object GameSaveAccountState {
    fun reset(context: Context) {
        val app = context.applicationContext
        runCatching { ScopeTombstoneStore(app).clearAll() }
        runCatching { CloudVerdictStore(app).clearAll() }
        runCatching {
            app.getSharedPreferences("savevault_game_save_heads", Context.MODE_PRIVATE)
                .edit()
                .clear()
                .commit()
        }
        runCatching { PendingUploadStore(app).clear() }
        runCatching {
            val workManager = WorkManager.getInstance(app)
            workManager.cancelUniqueWork(AutoBackupScheduler.IMMEDIATE_WORK_NAME)
            workManager.cancelUniqueWork(PendingUploadScheduler.WORK_NAME)
            workManager.cancelUniqueWork(BatchWorker.UNIQUE_WORK_NAME)
        }
        val repository = runCatching { SnapshotRepository.forApp(app) }.getOrNull()
        runCatching { repository?.clearUploadedFlags() }
        runCatching { File(app.cacheDir, "game-save-head-cache").deleteRecursively() }
        runCatching {
            val keptLocalPaths = repository?.list()?.mapTo(mutableSetOf()) { it.localPath }.orEmpty()
            File(app.filesDir, SnapshotRepository.DIRECTORY).listFiles { file ->
                file.isFile &&
                    file.name.startsWith("remote-") &&
                    file.name.endsWith(".zip") &&
                    file.name !in keptLocalPaths
            }?.forEach { it.delete() }
        }
    }
}
