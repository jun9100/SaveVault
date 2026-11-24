package com.jun9100.savevault

/**
 * 跨设备个人元数据（自定义游戏译名、自动备份排除规则）的双向合并协调器，
 * 从 [SourceViewModel] 中剥离出来以降低状态耦合度并支持纯 JVM 单元测试。
 */
data class ProfileMetadataSyncOutcome(
    val remote: GameSaveProfileMetadata,
    val addedLocalTitles: Int,
    val mergedExcludedScopes: Set<String>,
) {
    fun summaryText(): String =
        "已同步云端译名与规则（${remote.customTitles.size} 条译名，${mergedExcludedScopes.size} 条排除" +
            "${if (addedLocalTitles > 0) "，本机新增 $addedLocalTitles 条" else ""}）"
}

object ProfileMetadataCoordinator {
    fun sync(
        localTitles: Map<String, String>,
        localExcluded: Set<String>,
        remoteMerge: (titles: Map<String, String>, excluded: Set<String>) -> Result<GameSaveProfileMetadata?>,
        applyRemoteTitles: (Map<String, String>) -> Int,
        applyRemoteExcluded: (Set<String>) -> Set<String>,
    ): Result<ProfileMetadataSyncOutcome?> {
        return remoteMerge(localTitles, localExcluded).map { merged ->
            if (merged == null) return@map null
            val added = applyRemoteTitles(merged.customTitles)
            val nextExcluded = if (merged.excludedScopes.isNotEmpty()) {
                applyRemoteExcluded(merged.excludedScopes)
            } else {
                localExcluded
            }
            ProfileMetadataSyncOutcome(
                remote = merged,
                addedLocalTitles = added,
                mergedExcludedScopes = nextExcluded,
            )
        }
    }
}
