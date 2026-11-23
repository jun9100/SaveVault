package com.jun9100.opensave

import android.content.Context
import androidx.work.WorkManager
import com.jun9100.opensave.activity.ActivityEntry
import com.jun9100.opensave.activity.ActivityKind
import com.jun9100.opensave.activity.ActivityLevel
import com.jun9100.opensave.activity.ActivityRecorder
import com.jun9100.opensave.batch.BatchWorker

internal const val AUTH_BLOCKED_TOKEN_REVOKED = "token_revoked"
internal const val AUTH_BLOCKED_SHARED_TOKEN_DISABLED = "shared_token_disabled"

/**
 * The cloud's verdict on this install's credential, kept beside the config.
 * It holds no token: the token stays in [GameSaveConfigStore]'s Keystore file.
 */
internal class GameSaveAuthStore(context: Context) {
    private val preferences = context.applicationContext.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)

    /** "" when the credential is usable, otherwise the server's blocking reason. */
    fun blocked(): String = preferences.getString(BLOCKED_KEY, "").orEmpty().takeIf {
        it == AUTH_BLOCKED_TOKEN_REVOKED || it == AUTH_BLOCKED_SHARED_TOKEN_DISABLED
    }.orEmpty()

    fun setBlocked(code: String) {
        preferences.edit().apply {
            if (code in BLOCKING_CODES) putString(BLOCKED_KEY, code) else remove(BLOCKED_KEY)
        }.apply()
    }

    fun sharedTokenWarning(): Boolean = preferences.getBoolean(WARNING_KEY, false)

    fun setSharedTokenWarning(seen: Boolean) {
        preferences.edit().putBoolean(WARNING_KEY, seen).apply()
    }

    /** New credentials were saved: both the block and the warning are stale. */
    fun clear() {
        preferences.edit().clear().apply()
    }

    private companion object {
        const val PREFERENCES = "opensave_game_save_auth"
        const val BLOCKED_KEY = "auth_blocked"
        const val WARNING_KEY = "shared_token_warning"
        val BLOCKING_CODES = setOf(AUTH_BLOCKED_TOKEN_REVOKED, AUTH_BLOCKED_SHARED_TOKEN_DISABLED)
    }
}

/**
 * The app's [GameSaveAuthObserver]: persists the verdict, stops background
 * sync and tells the user once. Retrying a revoked token can never succeed,
 * and a periodic worker would otherwise notify every 15 minutes.
 */
internal class GameSaveAuthGuard(context: Context) : GameSaveAuthObserver {
    private val appContext = context.applicationContext
    private val store = GameSaveAuthStore(appContext)

    override fun onAuthRejected(code: String) {
        onAuthRejected(code, rejectedToken = "")
    }

    override fun onAuthRejected(code: String, rejectedToken: String) {
        if (code != AUTH_BLOCKED_TOKEN_REVOKED && code != AUTH_BLOCKED_SHARED_TOKEN_DISABLED) return
        synchronized(LOCK) {
            // If an in-flight worker failed with an older token after the user already
            // signed in again with a fresh credential, ignore the stale 401 so we never
            // wipe the newly saved token.
            if (rejectedToken.isNotBlank()) {
                val currentKey = runCatching { GameSaveConfigStore(appContext).load().syncKey }.getOrNull()
                if (currentKey != null && currentKey != rejectedToken) return
            }
            // The same verdict from a second concurrent request must not notify again.
            if (store.blocked() == code) return
            store.setBlocked(code)
            // A revoked token can never work again: drop it now, so no later request
            // carries it. The account name stays for the login form.
            if (code == AUTH_BLOCKED_TOKEN_REVOKED) dropRevokedToken(rejectedToken)
            cancelBackgroundSync(appContext)
            ActivityRecorder.record(
                appContext,
                ActivityEntry.create(
                    ActivityKind.UPLOAD,
                    ActivityLevel.ERROR,
                    if (code == AUTH_BLOCKED_TOKEN_REVOKED) {
                        "登录失效，请重新登录"
                    } else {
                        "SaveVault 云端已停用共享同步密钥"
                    },
                    if (code == AUTH_BLOCKED_TOKEN_REVOKED) {
                        "自动同步已停止。请在 设置 › 云端 用账号密码重新登录，或用配对码重新连接"
                    } else {
                        "请在 设置 › 云端 用配对码连接本设备"
                    },
                    needsAction = true,
                ),
                notify = true,
            )
        }
    }

    /**
     * Clears the token but keeps the address and the account name: the next
     * login only asks for the password again.
     */
    private fun dropRevokedToken(rejectedToken: String) {
        runCatching {
            val configStore = GameSaveConfigStore(appContext)
            val config = configStore.load()
            if (config.syncKey.isBlank()) return@runCatching
            if (rejectedToken.isNotBlank() && config.syncKey != rejectedToken) return@runCatching
            configStore.save(config.copy(enabled = false, syncKey = ""))
        }
    }

    override fun onSharedTokenWarning() {
        store.setSharedTokenWarning(true)
    }

    companion object {
        private val LOCK = Any()

        private fun cancelBackgroundSync(appContext: Context) {
            runCatching {
                WorkManager.getInstance(appContext).cancelUniqueWork(AutoBackupScheduler.WORK_NAME)
            }
            runCatching {
                WorkManager.getInstance(appContext).cancelUniqueWork(AutoBackupScheduler.IMMEDIATE_WORK_NAME)
            }
            runCatching {
                WorkManager.getInstance(appContext).cancelUniqueWork(PendingUploadScheduler.WORK_NAME)
            }
            runCatching {
                WorkManager.getInstance(appContext).cancelUniqueWork(BatchWorker.UNIQUE_WORK_NAME)
            }
            PendingUploadStore(appContext).clear()
        }

        /**
         * Call after the user saved new credentials. The user's Wi-Fi auto
         * backup switch is left alone; only its scheduler is restored.
         */
        fun credentialsReplaced(context: Context) {
            val appContext = context.applicationContext
            GameSaveAuthStore(appContext).clear()
            AutoBackupScheduler(appContext).ensureScheduled()
        }

        /**
         * Call after explicit logout: drops any old verdict and cancels queued
         * background sync work instead of re-enqueuing periodic sync with an empty token.
         */
        fun loggedOut(context: Context) {
            val appContext = context.applicationContext
            synchronized(LOCK) {
                GameSaveAuthStore(appContext).clear()
                cancelBackgroundSync(appContext)
            }
        }
    }
}
