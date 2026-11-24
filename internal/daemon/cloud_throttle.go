package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/jun9100/savevault/internal/snapshot"
)

// cloudUploadThrottle coalesces automatic cloud uploads for frequently-saving
// games (such as Forza Horizon 5) in two complementary ways:
//
//  1. When CloudUploadOnExitOnly is enabled and the game has an active play
//     session, mid-session auto-snapshots are coalesced in memory and only the
//     latest snapshot uploads once the game exits.
//  2. When CloudUploadMinInterval > 0, automatic uploads for the same game are
//     coalesced within that window and the newest snapshot uploads when the
//     window elapses.
//
// Local snapshots are unaffected; manual snapshots always upload immediately.
type cloudUploadThrottle struct {
	mu       sync.Mutex
	pending  map[string]*pendingCloudUpload
	lastSent map[string]time.Time
	now      func() time.Time
	stopped  bool
}

type pendingCloudUpload struct {
	zipPath    string
	remoteName string
	timer      *time.Timer
}

func (t *cloudUploadThrottle) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (d *Daemon) startCloudUpload(zipPath, remoteFileName string) {
	if d.P2P.Pause.Paused() {
		d.holdUpload(zipPath, remoteFileName)
		return
	}
	d.uploads.Add()
	go d.runCloudUpload(zipPath, remoteFileName, "", d.Log)
}

func (d *Daemon) enqueueCloudUpload(zipPath, remoteFileName string) {
	if d.P2P.Pause.Paused() {
		d.holdUpload(zipPath, remoteFileName)
		return
	}

	gameID, _, snapID, ok := snapshot.ParseExportEntryName(remoteFileName)
	if !ok {
		d.startCloudUpload(zipPath, remoteFileName)
		return
	}
	snap, err := d.Store.GetSnapshot(snapID)
	isAuto := err == nil && snap.IsSystemAuto

	now := d.cloudThrottle.clock()

	// Manual snapshots (and safety copies) always upload immediately and
	// supersede any older deferred auto-snapshot for the same game.
	if !isAuto {
		d.cloudThrottle.mu.Lock()
		if old := d.cloudThrottle.pending[gameID]; old != nil {
			if old.timer != nil {
				old.timer.Stop()
			}
			delete(d.cloudThrottle.pending, gameID)
		}
		if d.cloudThrottle.lastSent == nil {
			d.cloudThrottle.lastSent = map[string]time.Time{}
		}
		d.cloudThrottle.lastSent[gameID] = now
		d.cloudThrottle.mu.Unlock()
		d.startCloudUpload(zipPath, remoteFileName)
		return
	}

	settings, err := d.Store.GetSettings()
	if err != nil {
		d.startCloudUpload(zipPath, remoteFileName)
		return
	}

	deferOnPlay := settings.CloudUploadOnExitOnly && !d.PlayingSince(gameID).IsZero()
	minInterval := time.Duration(settings.CloudUploadMinInterval) * time.Minute

	d.cloudThrottle.mu.Lock()
	if d.cloudThrottle.stopped {
		d.cloudThrottle.mu.Unlock()
		d.startCloudUpload(zipPath, remoteFileName)
		return
	}
	if d.cloudThrottle.pending == nil {
		d.cloudThrottle.pending = map[string]*pendingCloudUpload{}
	}
	if d.cloudThrottle.lastSent == nil {
		d.cloudThrottle.lastSent = map[string]time.Time{}
	}

	if deferOnPlay {
		existing := d.cloudThrottle.pending[gameID]
		if existing != nil {
			if existing.timer != nil {
				existing.timer.Stop()
				existing.timer = nil
			}
			existing.zipPath = zipPath
			existing.remoteName = remoteFileName
		} else {
			d.cloudThrottle.pending[gameID] = &pendingCloudUpload{
				zipPath:    zipPath,
				remoteName: remoteFileName,
			}
			d.Log.Log("info", fmt.Sprintf("deferring cloud uploads for %s while playing; the latest save will upload on exit", gameID))
		}
		d.cloudThrottle.mu.Unlock()
		return
	}

	if minInterval > 0 {
		if last := d.cloudThrottle.lastSent[gameID]; !last.IsZero() {
			if wait := last.Add(minInterval).Sub(now); wait > 0 {
				existing := d.cloudThrottle.pending[gameID]
				if existing != nil {
					existing.zipPath = zipPath
					existing.remoteName = remoteFileName
				} else {
					entry := &pendingCloudUpload{
						zipPath:    zipPath,
						remoteName: remoteFileName,
					}
					entry.timer = time.AfterFunc(wait, func() {
						d.FlushPendingCloudUpload(gameID)
					})
					d.cloudThrottle.pending[gameID] = entry
				}
				d.cloudThrottle.mu.Unlock()
				return
			}
		}
	}

	if old := d.cloudThrottle.pending[gameID]; old != nil {
		if old.timer != nil {
			old.timer.Stop()
		}
		delete(d.cloudThrottle.pending, gameID)
	}
	d.cloudThrottle.lastSent[gameID] = now
	d.cloudThrottle.mu.Unlock()
	d.startCloudUpload(zipPath, remoteFileName)
}

// FlushPendingCloudUpload sends the latest coalesced cloud upload for gameID,
// if one is waiting. Called when a play session ends or a throttle window fires.
func (d *Daemon) FlushPendingCloudUpload(gameID string) {
	d.cloudThrottle.mu.Lock()
	entry := d.cloudThrottle.pending[gameID]
	if entry == nil {
		d.cloudThrottle.mu.Unlock()
		return
	}
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	delete(d.cloudThrottle.pending, gameID)
	if d.cloudThrottle.lastSent == nil {
		d.cloudThrottle.lastSent = map[string]time.Time{}
	}
	d.cloudThrottle.lastSent[gameID] = d.cloudThrottle.clock()
	zipPath, remoteName := entry.zipPath, entry.remoteName
	d.cloudThrottle.mu.Unlock()

	if !snapshot.ArchiveExists(zipPath) {
		return
	}
	d.startCloudUpload(zipPath, remoteName)
}

// FlushAllPendingCloudUploads sends every currently deferred/throttled upload
// and stops all pending timers. When shutdown is true, future calls bypass
// deferral so Stop can drain cleanly.
func (d *Daemon) FlushAllPendingCloudUploads(shutdown bool) {
	d.cloudThrottle.mu.Lock()
	if shutdown {
		d.cloudThrottle.stopped = true
	}
	if len(d.cloudThrottle.pending) == 0 {
		d.cloudThrottle.mu.Unlock()
		return
	}
	now := d.cloudThrottle.clock()
	if d.cloudThrottle.lastSent == nil {
		d.cloudThrottle.lastSent = map[string]time.Time{}
	}
	toSend := make([]heldUpload, 0, len(d.cloudThrottle.pending))
	for gameID, entry := range d.cloudThrottle.pending {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		d.cloudThrottle.lastSent[gameID] = now
		toSend = append(toSend, heldUpload{zipPath: entry.zipPath, remoteName: entry.remoteName})
	}
	d.cloudThrottle.pending = map[string]*pendingCloudUpload{}
	d.cloudThrottle.mu.Unlock()

	for _, item := range toSend {
		if !snapshot.ArchiveExists(item.zipPath) {
			continue
		}
		d.startCloudUpload(item.zipPath, item.remoteName)
	}
}
