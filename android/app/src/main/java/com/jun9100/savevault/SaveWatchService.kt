// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package com.jun9100.savevault

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.FileObserver
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import androidx.core.app.NotificationCompat
import com.jun9100.savevault.ops.ready
import java.io.File
import java.nio.file.Files

/**
 * Watches the save directories the library already knows about and asks for a
 * backup as soon as one of them changes, so a save reaches the cloud seconds
 * after the emulator writes it instead of waiting for the next 15-minute round.
 *
 * The periodic worker stays in place as the fallback: this service only runs
 * while the user keeps "存档变化立即上传" on, and Android may stop it at any
 * time, after which the periodic round picks the change up again.
 */
class SaveWatchService : Service() {
    private val handler = Handler(Looper.getMainLooper())
    private val observers = mutableListOf<FileObserver>()
    private var watched = listOf<String>()
    @Volatile
    private var destroyed = false

    private val flush = Runnable {
        if (destroyed || !SaveWatchPolicy.shouldUpload(applicationContext)) return@Runnable
        AutoBackupScheduler(applicationContext).requestImmediateBackup(REASON_WATCH)
    }

    override fun onCreate() {
        super.onCreate()
        destroyed = false
        startForegroundWithNotification()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopSelf()
                return START_NOT_STICKY
            }
            else -> {
                if (!SaveWatchPolicy.shouldUpload(applicationContext)) {
                    stopSelf()
                    return START_NOT_STICKY
                }
                val dirs = if (intent == null) {
                    SaveWatchPolicy.watchedDirectories(applicationContext)
                } else {
                    intent.getStringArrayListExtra(EXTRA_DIRS).orEmpty().also {
                        SaveWatchPolicy.saveWatchedDirectories(applicationContext, it)
                    }
                }
                val watchTargets = resolveWatchTargets(dirs)
                if (watchTargets.isEmpty()) {
                    stopObservers()
                    watched = emptyList()
                    stopSelf()
                    return START_NOT_STICKY
                }
                if (watchTargets != watched) {
                    stopObservers()
                    startObservers(watchTargets)
                }
                startForegroundWithNotification()
            }
        }
        return START_STICKY
    }

    override fun onDestroy() {
        destroyed = true
        stopObservers()
        handler.removeCallbacks(flush)
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun resolveWatchTargets(dirs: List<String>): List<String> {
        val resolved = LinkedHashSet<String>()
        for (dir in dirs) {
            if (resolved.size >= MAX_OBSERVERS) break
            val root = File(dir)
            if (!root.isDirectory || !root.canRead() || Files.isSymbolicLink(root.toPath())) continue
            resolved += root.path
            // FileObserver is non-recursive: also watch immediate readable subdirectories so
            // nested slot folders inside a save root trigger immediate uploads.
            root.listFiles()?.forEach { child ->
                if (resolved.size >= MAX_OBSERVERS) return@forEach
                if (child.isDirectory && child.canRead() && !Files.isSymbolicLink(child.toPath())) {
                    resolved += child.path
                }
            }
        }
        return resolved.toList()
    }

    private fun startObservers(dirs: List<String>) {
        val active = mutableListOf<String>()
        for (dir in dirs) {
            val observer = object : FileObserver(dir, EVENTS) {
                override fun onEvent(event: Int, path: String?) {
                    if (destroyed || (event and EVENTS) == 0) return
                    // Any write, rename or delete inside a save directory means
                    // the game is saving; debounce so a burst becomes one upload.
                    handler.removeCallbacks(flush)
                    handler.postDelayed(flush, DEBOUNCE_MILLIS)
                }
            }
            runCatching { observer.startWatching() }.onSuccess {
                observers += observer
                active += dir
            }
        }
        watched = active
    }

    private fun stopObservers() {
        observers.forEach { runCatching { it.stopWatching() } }
        observers.clear()
    }

    private fun startForegroundWithNotification() {
        val manager = getSystemService(NotificationManager::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O &&
            manager?.getNotificationChannel(CHANNEL_ID) == null
        ) {
            manager?.createNotificationChannel(
                NotificationChannel(CHANNEL_ID, "实时备份", NotificationManager.IMPORTANCE_LOW),
            )
        }
        val notification = NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_savevault)
            .setContentTitle("SaveVault 实时备份")
            .setContentText("正在监视 ${watched.size} 个存档位置，存档变化后自动上传")
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
    }

    companion object {
        private const val CHANNEL_ID = "savevault_save_watch"
        private const val NOTIFICATION_ID = 4713
        private const val DEBOUNCE_MILLIS = 15_000L
        private const val MAX_OBSERVERS = 128
        private const val REASON_WATCH = "watch"
        const val ACTION_STOP = "com.jun9100.savevault.STOP_SAVE_WATCH"
        const val EXTRA_DIRS = "com.jun9100.savevault.SAVE_WATCH_DIRS"

        private const val EVENTS = FileObserver.CREATE or FileObserver.MODIFY or
            FileObserver.CLOSE_WRITE or FileObserver.MOVED_TO or FileObserver.DELETE or
            FileObserver.MOVED_FROM

        fun start(context: Context, directories: Collection<String>) {
            val unique = directories.distinct()
            SaveWatchPolicy.saveWatchedDirectories(context.applicationContext, unique)
            val intent = Intent(context, SaveWatchService::class.java)
                .putStringArrayListExtra(EXTRA_DIRS, ArrayList(unique))
            context.startForegroundService(intent)
        }

        fun stop(context: Context) {
            context.stopService(Intent(context, SaveWatchService::class.java))
        }
    }
}

/** Keeps the service honest about the switches on the settings page. */
internal object SaveWatchPolicy {
    private const val PREFERENCES = "savevault_automation"
    private const val WATCH_KEY = "save_watch_enabled"
    private const val WATCHED_DIRS_KEY = "save_watch_dirs"

    fun isEnabled(context: Context): Boolean = context.applicationContext
        .getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
        .getBoolean(WATCH_KEY, false)

    fun setEnabled(context: Context, enabled: Boolean) {
        context.applicationContext.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
            .edit().putBoolean(WATCH_KEY, enabled).apply()
    }

    fun watchedDirectories(context: Context): List<String> = context.applicationContext
        .getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
        .getStringSet(WATCHED_DIRS_KEY, emptySet())
        .orEmpty()
        .sorted()

    fun saveWatchedDirectories(context: Context, directories: Collection<String>) {
        val editor = context.applicationContext
            .getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
            .edit()
        val unique = directories.filter(String::isNotBlank).toSet()
        if (unique.isEmpty()) editor.remove(WATCHED_DIRS_KEY) else editor.putStringSet(WATCHED_DIRS_KEY, unique)
        editor.apply()
    }

    /** A watch event turns into an upload whenever watch is on and cloud upload is ready. */
    fun shouldUpload(context: Context): Boolean {
        val app = context.applicationContext
        val preferences = app.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
        return preferences.getBoolean(WATCH_KEY, false) &&
            GameSaveConfigStore(app).load().ready
    }
}
