package com.jun9100.savevault.ui

import android.Manifest
import android.app.Activity
import android.content.Context
import android.content.ContextWrapper
import android.content.Intent
import android.os.Build
import android.provider.Settings
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.focusGroup
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.WindowInsetsSides
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.only
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material.icons.outlined.FolderOpen
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material.icons.outlined.Sync
import androidx.compose.material.icons.outlined.SportsEsports
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.NavigationRail
import androidx.compose.material3.NavigationRailItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.platform.testTag
import androidx.core.view.WindowCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import com.jun9100.savevault.BackTarget
import com.jun9100.savevault.LibraryView
import com.jun9100.savevault.SaveVaultPage
import com.jun9100.savevault.SourceViewModel
import com.jun9100.savevault.activity.ActivityNotifications
import com.jun9100.savevault.ui.onboarding.FirstRunWizard
import com.jun9100.savevault.ui.onboarding.WizardActions
import com.jun9100.savevault.ui.settings.readClipboardText
import com.jun9100.savevault.ui.batch.BatchPlanDialog
import com.jun9100.savevault.ui.batch.BatchReportSheet
import com.jun9100.savevault.ui.batch.BatchStripState
import com.jun9100.savevault.ui.batch.LocalBatchStrip
import com.jun9100.savevault.ui.dialogs.CloudSnapshotDeleteConfirmDialog
import com.jun9100.savevault.ui.dialogs.GameBackupConfirmDialog
import com.jun9100.savevault.ui.dialogs.KeepLocalConfirmDialog
import com.jun9100.savevault.ui.dialogs.OtherDeviceRestoreDialog
import com.jun9100.savevault.ui.dialogs.RestoreConfirmDialog
import com.jun9100.savevault.ui.dialogs.WorkspaceWriteBackDialog
import com.jun9100.savevault.ui.input.FocusWatch
import com.jun9100.savevault.ui.input.HandheldCommandEffect
import com.jun9100.savevault.ui.input.HandheldDispatch
import com.jun9100.savevault.ui.input.HandheldHints
import com.jun9100.savevault.ui.input.LocalShellFocus
import com.jun9100.savevault.ui.input.busyDimmed
import com.jun9100.savevault.ui.input.handheldFocusRing
import com.jun9100.savevault.ui.input.handheldNavigationActive
import com.jun9100.savevault.ui.input.watchFocus
import com.jun9100.savevault.ui.input.whenIdle
import com.jun9100.savevault.ui.library.GameLibraryPage
import com.jun9100.savevault.ui.settings.CloudDevicesDialog
import com.jun9100.savevault.ui.settings.ConfigImportConfirmDialog
import com.jun9100.savevault.ui.settings.GameSaveEditorDialog
import com.jun9100.savevault.ui.settings.GameSaveTestFailedDialog
import com.jun9100.savevault.ui.settings.PairingConfirmDialog
import com.jun9100.savevault.ui.settings.PairingDialog
import com.jun9100.savevault.ui.settings.RevokeDeviceConfirmDialog
import com.jun9100.savevault.ui.settings.SettingsPage
import com.jun9100.savevault.ui.sources.SourcesPage
import com.jun9100.savevault.ui.sync.SyncTasksPage

@Composable
internal fun SaveVaultScreen(sourceViewModel: SourceViewModel = viewModel()) {
    val state by sourceViewModel.uiState.collectAsStateWithLifecycle()
    // Back from an emulator: saves may have changed, so the status badges re-read their signatures.
    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { sourceViewModel.refreshSaveSignatures() }
    val folderPicker = rememberLauncherForActivityResult(
        contract = ActivityResultContracts.OpenDocumentTree(),
        onResult = { uri -> uri?.let(sourceViewModel::onTreeSelected) },
    )
    // Hoisted here so switching pages or between portrait and landscape keeps the scroll position.
    val libraryListState = rememberLazyListState()
    val context = LocalContext.current
    val notifications = rememberNotificationAccess(
        asked = state.notificationsAsked,
        onAsked = sourceViewModel::markNotificationsAsked,
    )

    // 设置 shows the credential's state; ask on entry so a pairing done elsewhere or a
    // revocation seen by a worker is visible without restarting the app.
    LaunchedEffect(state.page) {
        if (state.page == SaveVaultPage.SETTINGS) sourceViewModel.refreshGameSaveAuth()
    }

    // First, so the dialogs it opens (game-save editor, import, test failure) come up on top of it.
    state.wizardStep?.let { step ->
        FirstRunWizard(
            state = state,
            step = step,
            notificationsAllowed = notifications.allowed,
            actions = WizardActions(
                onPickFolder = { folderPicker.launch(null) },
                onPickFolderAt = { uri -> folderPicker.launch(uri) },
                onChooseRoot = sourceViewModel::chooseRootAccess,
                onChooseSaf = sourceViewModel::chooseSafAccess,
                onEditCloud = sourceViewModel::openGameSaveEditor,
                onImportFromClipboard = { sourceViewModel.importGameSaveConfig(readClipboardText(context)) },
                onRequestNotifications = notifications.request,
                onOpenNotificationSettings = notifications.openSettings,
                onBack = sourceViewModel::wizardBack,
                onNext = sourceViewModel::wizardNext,
                onSkip = sourceViewModel::finishWizard,
            ),
        )
    }

    state.pendingGameBackup?.let { entry ->
        GameBackupConfirmDialog(
            entry = entry,
            onDismiss = sourceViewModel::cancelGameBackup,
            onConfirm = { includeCandidates -> sourceViewModel.confirmGameBackup(includeCandidates = includeCandidates) },
        )
    }

    state.pendingWriteBack?.let { request ->
        WorkspaceWriteBackDialog(
            request = request,
            onDismiss = sourceViewModel::cancelWorkspaceWriteBack,
            onConfirm = sourceViewModel::confirmWorkspaceWriteBack,
        )
    }

    state.pendingRestore?.let { request ->
        RestoreConfirmDialog(
            request = request,
            storageMode = state.storageMode,
            onDismiss = sourceViewModel::cancelRestore,
            onConfirm = sourceViewModel::confirmRestore,
        )
    }

    state.pendingOtherDeviceRestore?.let { request ->
        OtherDeviceRestoreDialog(
            request = request,
            onChoose = sourceViewModel::chooseOtherDeviceTarget,
            onDismiss = sourceViewModel::cancelOtherDeviceRestore,
            onConfirm = sourceViewModel::confirmOtherDeviceRestore,
        )
    }

    state.pendingBatchPlan?.let { plan ->
        BatchPlanDialog(
            plan = plan,
            onDismiss = sourceViewModel::cancelBatchPlan,
            onConfirm = sourceViewModel::confirmBatchPlan,
        )
    }

    state.pendingKeepLocal?.let { request ->
        KeepLocalConfirmDialog(
            request = request,
            onDismiss = sourceViewModel::cancelKeepLocal,
            onConfirm = sourceViewModel::confirmKeepLocal,
        )
    }

    state.gameSaveEditor?.let { initial ->
        GameSaveEditorDialog(
            initial = initial,
            busy = state.busy,
            onDismiss = sourceViewModel::closeGameSaveEditor,
            onLogin = sourceViewModel::loginAndSaveGameSave,
            initialUsername = state.gameSaveUsername,
        )
    }

    state.pairingDraft?.let { draft ->
        PairingDialog(
            draft = draft,
            busy = state.busy,
            onDismiss = sourceViewModel::closePairing,
            onSubmit = sourceViewModel::submitPairingDraft,
            onPaste = { text -> sourceViewModel.openPairingLink(text.orEmpty()) },
        )
    }

    state.pendingPairing?.let { link ->
        PairingConfirmDialog(
            link = link,
            busy = state.busy,
            onDismiss = sourceViewModel::cancelPairing,
            onConfirm = sourceViewModel::confirmPairing,
        )
    }

    state.gameSaveTestFailure?.let { failure ->
        GameSaveTestFailedDialog(
            failure = failure,
            busy = state.busy,
            onSaveAnyway = sourceViewModel::saveGameSaveAnyway,
            onEdit = sourceViewModel::editFailedGameSave,
            onDismiss = sourceViewModel::dismissGameSaveTestFailure,
        )
    }

    state.pendingConfigImport?.let { config ->
        ConfigImportConfirmDialog(
            config = config,
            replacing = state.gameSaveUrl.isNotBlank() || state.gameSaveSyncKey.isNotBlank(),
            busy = state.busy,
            onDismiss = sourceViewModel::cancelConfigImport,
            onConfirm = sourceViewModel::confirmConfigImport,
        )
    }

    state.cloudDevicesDialog?.let { dialog ->
        CloudDevicesDialog(
            dialog = dialog,
            busy = state.busy,
            onRefresh = sourceViewModel::refreshCloudDevices,
            onRequestRevoke = sourceViewModel::requestRevokeCloudDevice,
            onDismiss = sourceViewModel::closeCloudDevices,
        )
        dialog.pendingRevoke?.let { token ->
            RevokeDeviceConfirmDialog(
                token = token,
                isCurrentDevice = dialog.report?.currentTokenId?.let { it.isNotBlank() && token.id == it } == true,
                busy = state.busy,
                onDismiss = sourceViewModel::cancelRevokeCloudDevice,
                onConfirm = sourceViewModel::confirmRevokeCloudDevice,
            )
        }
    }

    state.pendingCloudSnapshotDelete?.let { request ->
        CloudSnapshotDeleteConfirmDialog(
            request = request,
            onDismiss = sourceViewModel::cancelDeleteCloudSnapshot,
            onConfirm = sourceViewModel::confirmDeleteCloudSnapshot,
        )
    }

    (state.viewedBatchReport ?: state.lastBatchReport)?.takeIf { state.batchReportOpen }?.let { report ->
        BatchReportSheet(
            report = report,
            busy = state.busy,
            onRetryFailed = sourceViewModel::retryFailedBatch,
            onDismiss = sourceViewModel::closeBatchReport,
        )
    }

    val batchStrip = remember(state.batch, state.lastBatchReport, state.batchReportUnseen) {
        BatchStripState(
            batch = state.batch,
            report = state.lastBatchReport?.takeIf { state.batchReportUnseen },
            onCancel = sourceViewModel::cancelBatch,
            onOpenReport = sourceViewModel::openBatchReport,
            onDismissReport = sourceViewModel::dismissBatchReport,
        )
    }

    // L1/R1 switch the top-level page from anywhere, even with nothing focused.
    HandheldCommandEffect { command ->
        HandheldDispatch.pageAfter(state.navigationPage, command)?.let(sourceViewModel::showPage)
    }

    val shellFocus = remember { FocusWatch() }
    ProvideAdaptiveLayout(modifier = Modifier.fillMaxSize().watchFocus(shellFocus)) {
        CompositionLocalProvider(LocalShellFocus provides shellFocus, LocalBatchStrip provides batchStrip) {
            val landscape = LocalAdaptiveLayout.current.isLandscape
            ImmersiveSystemBarsEffect(hide = landscape && state.immersiveLandscape)
            // Back stack: portrait game detail -> list; secondary page -> library; otherwise the system.
            BackHandler(enabled = LibraryView.backTarget(state.page, state.gameDetailOpen, landscape) != BackTarget.SYSTEM) {
                sourceViewModel.navigateBack(landscape)
            }
            AdaptiveShellFrame(
                page = state.page,
                navigationPage = state.navigationPage,
                busy = state.busy,
                progress = LibraryView.progressLine(state),
                gameDetailOpen = state.gameDetailOpen && state.selectedGameKey != null,
                onShowPage = sourceViewModel::showPage,
                onRefresh = {
                    when (state.page) {
                        SaveVaultPage.LIBRARY, SaveVaultPage.SOURCES -> sourceViewModel.scanSource()
                        SaveVaultPage.SETTINGS -> {
                            sourceViewModel.refreshPersistedSource()
                            sourceViewModel.refreshGameSaveAuth()
                        }
                        SaveVaultPage.SYNC -> sourceViewModel.checkCloudUpdates(SaveVaultPage.SYNC)
                    }
                },
                onCloseDetail = sourceViewModel::closeGameDetail,
                onNavigateUp = { sourceViewModel.navigateBack(landscape) },
            ) { contentModifier ->
                when (state.page) {
                    SaveVaultPage.LIBRARY -> GameLibraryPage(
                        state = state,
                        viewModel = sourceViewModel,
                        listState = libraryListState,
                        modifier = contentModifier,
                        onOpenSources = { sourceViewModel.showPage(SaveVaultPage.SOURCES) },
                        onPickFolder = { folderPicker.launch(null) },
                    )
                    SaveVaultPage.SOURCES -> SourcesPage(
                        state = state,
                        viewModel = sourceViewModel,
                        onPickFolder = { folderPicker.launch(null) },
                        onPickFolderAt = { uri -> folderPicker.launch(uri) },
                        modifier = contentModifier,
                    )
                    SaveVaultPage.SETTINGS -> SettingsPage(
                        state = state,
                        viewModel = sourceViewModel,
                        notifications = notifications,
                        modifier = contentModifier,
                    )
                    SaveVaultPage.SYNC -> SyncTasksPage(
                        state = state,
                        viewModel = sourceViewModel,
                        modifier = contentModifier,
                    )
                }
            }
        }
    }
}

/**
 * The navigation frame without the ViewModel. Landscape: NavigationRail on the
 * left and no TopAppBar, so the page keeps the whole height. Portrait: TopAppBar
 * and NavigationBar; the full-screen game detail swaps them for a back arrow.
 * The status strip is drawn under the page only while [busy]; in landscape the
 * library page draws its own under the detail pane instead.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun AdaptiveShellFrame(
    page: SaveVaultPage,
    busy: Boolean,
    progress: String?,
    gameDetailOpen: Boolean,
    onShowPage: (SaveVaultPage) -> Unit,
    onRefresh: () -> Unit,
    onCloseDetail: () -> Unit,
    /** The rail or bottom bar item to highlight; a secondary page highlights the one it came from. */
    navigationPage: SaveVaultPage = page,
    /** Portrait back arrow of a secondary page. */
    onNavigateUp: () -> Unit = {},
    content: @Composable ColumnScope.(Modifier) -> Unit,
) {
    val landscape = LocalAdaptiveLayout.current.isLandscape
    if (landscape) {
        Surface(modifier = Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
            Row(modifier = Modifier.fillMaxSize()) {
                NavigationRail(
                    modifier = Modifier.fillMaxHeight().testTag(NAVIGATION_RAIL_TAG).focusRestorer().focusGroup(),
                    header = {
                        Text(
                            "SaveVault",
                            style = MaterialTheme.typography.labelMedium,
                            color = MaterialTheme.colorScheme.primary,
                            maxLines = 1,
                        )
                    },
                ) {
                    SaveVaultPage.topLevelPages.forEach { item ->
                        NavigationRailItem(
                            selected = navigationPage == item,
                            onClick = { onShowPage(item) },
                            modifier = Modifier.handheldFocusRing(RoundedCornerShape(16.dp)),
                            icon = { Icon(pageIcon(item), contentDescription = null) },
                            label = { Text(item.title, maxLines = 1, overflow = TextOverflow.Ellipsis) },
                        )
                    }
                    Spacer(Modifier.weight(1f))
                    NavigationRailItem(
                        selected = false,
                        onClick = whenIdle(busy, onRefresh),
                        modifier = Modifier.handheldFocusRing(RoundedCornerShape(16.dp)).busyDimmed(busy),
                        icon = { Icon(Icons.Outlined.Refresh, contentDescription = "刷新") },
                        label = { Text(refreshLabel(page)) },
                    )
                    if (handheldNavigationActive()) ButtonHints(page)
                }
                Column(
                    modifier = Modifier
                        .weight(1f)
                        .fillMaxHeight()
                        .windowInsetsPadding(WindowInsets.safeDrawing.only(WindowInsetsSides.Vertical + WindowInsetsSides.End)),
                ) {
                    content(Modifier.weight(1f))
                    // The library draws it under its detail pane so the list keeps its height.
                    if (page != SaveVaultPage.LIBRARY) StatusStrip(busy = busy, progress = progress)
                }
            }
        }
    } else {
        val showingDetail = page == SaveVaultPage.LIBRARY && gameDetailOpen
        val secondary = !page.topLevel
        Scaffold(
            topBar = {
                TopAppBar(
                    navigationIcon = {
                        if (showingDetail) {
                            IconButton(onClick = onCloseDetail, modifier = Modifier.handheldFocusRing(CircleShape)) {
                                Icon(Icons.AutoMirrored.Outlined.ArrowBack, contentDescription = "返回游戏列表")
                            }
                        } else if (secondary) {
                            IconButton(onClick = onNavigateUp, modifier = Modifier.handheldFocusRing(CircleShape)) {
                                Icon(Icons.AutoMirrored.Outlined.ArrowBack, contentDescription = "返回")
                            }
                        }
                    },
                    title = {
                        Column {
                            Text("SaveVault")
                            Text(
                                text = if (showingDetail) "游戏详情" else page.title,
                                style = MaterialTheme.typography.labelSmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                    },
                    actions = {
                        if (!showingDetail) {
                            IconButton(
                                onClick = whenIdle(busy, onRefresh),
                                modifier = Modifier.handheldFocusRing(CircleShape).busyDimmed(busy),
                            ) {
                                Icon(Icons.Outlined.Refresh, contentDescription = "刷新")
                            }
                        }
                    },
                )
            },
            bottomBar = {
                if (!showingDetail) {
                    NavigationBar {
                        SaveVaultPage.topLevelPages.forEach { item ->
                            NavigationBarItem(
                                selected = navigationPage == item,
                                onClick = { onShowPage(item) },
                                modifier = Modifier.handheldFocusRing(RoundedCornerShape(16.dp)),
                                icon = { Icon(pageIcon(item), contentDescription = null) },
                                label = { Text(item.title) },
                            )
                        }
                    }
                }
            },
        ) { padding ->
            Column(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(padding)
                    .consumeWindowInsets(padding)
                    .imePadding(),
            ) {
                content(Modifier.weight(1f))
                StatusStrip(busy = busy, progress = progress)
            }
        }
    }
}

private fun pageIcon(page: SaveVaultPage): ImageVector = when (page) {
    SaveVaultPage.LIBRARY -> Icons.Outlined.SportsEsports
    SaveVaultPage.SOURCES -> Icons.Outlined.FolderOpen
    SaveVaultPage.SETTINGS -> Icons.Outlined.Settings
    SaveVaultPage.SYNC -> Icons.Outlined.Sync
}

private fun refreshLabel(page: SaveVaultPage): String = when (page) {
    SaveVaultPage.LIBRARY, SaveVaultPage.SOURCES -> "扫描"
    SaveVaultPage.SETTINGS -> "刷新"
    SaveVaultPage.SYNC -> "检查"
}

/** Compact key legend under the rail items; shown only while the pad is in use. */
@Composable
private fun ButtonHints(page: SaveVaultPage) {
    Column(
        modifier = Modifier.padding(top = 8.dp, bottom = 4.dp).testTag(BUTTON_HINTS_TAG),
        verticalArrangement = Arrangement.spacedBy(1.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        HandheldHints.forPage(page).forEach { (key, action) ->
            Text(
                "$key $action",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
            )
        }
    }
}

internal const val NAVIGATION_RAIL_TAG = "navigation-rail"
internal const val LIBRARY_TOOLBAR_TAG = "library-toolbar"
internal const val GAME_DETAIL_TAG = "game-detail"
internal const val BUTTON_HINTS_TAG = "button-hints"

/**
 * 横屏沉浸模式 (设置 → 显示, plan Q2): hides the status and gesture bars while
 * [hide]; a swipe from the edge shows them for a moment. Leaving landscape or
 * switching it off shows them again.
 */
@Composable
private fun ImmersiveSystemBarsEffect(hide: Boolean) {
    val view = LocalView.current
    DisposableEffect(hide, view) {
        val window = view.context.findActivity()?.window
        if (window != null) {
            val controller = WindowCompat.getInsetsController(window, view)
            if (hide) {
                controller.systemBarsBehavior = WindowInsetsControllerCompat.BEHAVIOR_SHOW_TRANSIENT_BARS_BY_SWIPE
                controller.hide(WindowInsetsCompat.Type.systemBars())
            } else {
                controller.show(WindowInsetsCompat.Type.systemBars())
            }
        }
        onDispose { }
    }
}

/**
 * Whether notifications may be posted, and the one way to ask: the runtime
 * permission (Android 13+) is requested at most once, from the wizard or 设置;
 * after that only the system settings can change it.
 */
internal class NotificationAccess(
    val allowed: Boolean,
    /** Null when there is nothing left to ask. */
    val request: (() -> Unit)?,
    val openSettings: () -> Unit,
)

@Composable
private fun rememberNotificationAccess(asked: Boolean, onAsked: () -> Unit): NotificationAccess {
    val context = LocalContext.current
    var allowed by remember { mutableStateOf(ActivityNotifications.allowed(context)) }
    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { allowed = ActivityNotifications.allowed(context) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        allowed = granted && ActivityNotifications.allowed(context)
    }
    val canAsk = !asked && !allowed && Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
    return NotificationAccess(
        allowed = allowed,
        request = if (canAsk) {
            {
                onAsked()
                launcher.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        } else {
            null
        },
        openSettings = {
            runCatching {
                context.startActivity(
                    Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                        .putExtra(Settings.EXTRA_APP_PACKAGE, context.packageName)
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                )
            }
        },
    )
}

private tailrec fun Context.findActivity(): Activity? = when (this) {
    is Activity -> this
    is ContextWrapper -> baseContext.findActivity()
    else -> null
}
