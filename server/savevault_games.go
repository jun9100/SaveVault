package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Games group SaveVault (SaveVault) scopes of one type that hold saves of the
// same game, so a phone's and a PC's scopes of one title show as one game.
// Grouping is an index over scopes: it never moves a head, never mixes save
// types and never restores anything. See docs/C1_GAME_GROUPS.md.

const maxGameNameRunes = 120

var (
	switchTitleIDPattern = regexp.MustCompile(`^[0-9A-Fa-f]{16}$`)
	switchScopeIDPattern = regexp.MustCompile(`^switch-([0-9A-Fa-f]{16})$`)
	steamAppIDPattern    = regexp.MustCompile(`^[0-9]{1,12}$`)
	pspGameIDPattern     = regexp.MustCompile(`^[A-Z]{4}[0-9]{5}$`)
	androidPackagePat    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	gameKeyValuePattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,160}$`)
)

// saveVaultScopeIdentity is what a client knows about the game behind a scope.
type saveVaultScopeIdentity struct {
	Name           string    `json:"name,omitempty"`
	TitleID        string    `json:"titleId,omitempty"`
	SteamAppID     string    `json:"steamAppId,omitempty"`
	PSPGameID      string    `json:"pspGameId,omitempty"`
	AndroidPackage string    `json:"androidPackage,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt,omitzero"`
}

func (identity saveVaultScopeIdentity) sameFacts(other saveVaultScopeIdentity) bool {
	return identity.Name == other.Name && identity.TitleID == other.TitleID &&
		identity.SteamAppID == other.SteamAppID && identity.PSPGameID == other.PSPGameID &&
		identity.AndroidPackage == other.AndroidPackage
}

func validGameName(name string) bool {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxGameNameRunes {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func normalizeScopeIdentity(identity saveVaultScopeIdentity) (saveVaultScopeIdentity, bool) {
	identity.Name = strings.TrimSpace(identity.Name)
	identity.TitleID = strings.ToUpper(strings.TrimSpace(identity.TitleID))
	identity.SteamAppID = strings.TrimSpace(identity.SteamAppID)
	identity.PSPGameID = strings.ToUpper(strings.TrimSpace(identity.PSPGameID))
	identity.AndroidPackage = strings.TrimSpace(identity.AndroidPackage)
	identity.UpdatedAt = time.Time{}
	ok := validGameName(identity.Name) &&
		(identity.TitleID == "" || switchTitleIDPattern.MatchString(identity.TitleID)) &&
		(identity.SteamAppID == "" || steamAppIDPattern.MatchString(identity.SteamAppID)) &&
		(identity.PSPGameID == "" || pspGameIDPattern.MatchString(identity.PSPGameID)) &&
		(identity.AndroidPackage == "" || (len(identity.AndroidPackage) <= 128 && androidPackagePat.MatchString(identity.AndroidPackage)))
	return identity, ok
}

// Game link sources, from the strongest.
const (
	gameLinkManual = "manual"
	gameLinkAuto   = "auto"
	gameLinkNone   = "none"
)

// saveVaultGameKey is the game a scope belongs to: a hand-made link first,
// then the strongest identity fact, then a Switch scope named after its
// TitleID, and otherwise the scope alone. Every key starts with the scope's
// type, so games never span save types.
func saveVaultGameKey(kind, gameID string, stored saveVaultScopeState) (string, string) {
	if stored.GameLink != "" {
		return stored.GameLink, gameLinkManual
	}
	if identity := stored.Identity; identity != nil {
		switch {
		case kind == "switch" && identity.TitleID != "":
			return kind + "/title:" + identity.TitleID, gameLinkAuto
		case identity.SteamAppID != "":
			return kind + "/steam:" + identity.SteamAppID, gameLinkAuto
		case identity.PSPGameID != "":
			return kind + "/psp:" + identity.PSPGameID, gameLinkAuto
		case identity.AndroidPackage != "":
			return kind + "/android:" + identity.AndroidPackage, gameLinkAuto
		}
	}
	if kind == "switch" {
		if match := switchScopeIDPattern.FindStringSubmatch(gameID); match != nil {
			return kind + "/title:" + strings.ToUpper(match[1]), gameLinkAuto
		}
	}
	return kind + "/scope:" + gameID, gameLinkNone
}

// validManualGameKey accepts a key of the scope's own type.
func validManualGameKey(kind, key string) bool {
	rest, found := strings.CutPrefix(key, kind+"/")
	return found && gameKeyValuePattern.MatchString(rest)
}

type saveVaultGameMember struct {
	Type           string                 `json:"type"`
	GameID         string                 `json:"gameId"`
	Link           string                 `json:"link"`
	Name           string                 `json:"name,omitempty"`
	Head           *saveVaultScopeSnapshot `json:"head"`
	SnapshotCount  int                    `json:"snapshotCount"`
	ConflictCount  int                    `json:"conflictCount"`
	ArchiveBytes   int64                  `json:"archiveBytes"`
	LastReceivedAt time.Time              `json:"lastReceivedAt,omitzero"`
	Retired        bool                   `json:"retired,omitempty"`
}

type saveVaultGame struct {
	Key            string               `json:"key"`
	Type           string               `json:"type"`
	Name           string               `json:"name"`
	Meta           saveVaultGameMeta     `json:"meta"`
	Scopes         []saveVaultGameMember `json:"scopes"`
	LastReceivedAt time.Time            `json:"lastReceivedAt,omitzero"`
}

// listSaveVaultGamesLocked groups every scope into games, the most recently
// active first. The caller holds saveVaultMu.
func (s *server) listSaveVaultGamesLocked() []saveVaultGame {
	return s.listSaveVaultGamesForAdminLocked(false)
}

// listSaveVaultGamesForAdminLocked is listSaveVaultGamesLocked with the cover
// URL namespace selected by admin. The caller holds saveVaultMu; this function
// acquires metaMu after it, following the documented saveVaultMu -> metaMu
// order.
func (s *server) listSaveVaultGamesForAdminLocked(admin bool) []saveVaultGame {
	byKey := make(map[string]*saveVaultGame)
	for key, stored := range s.saveVaultState.Scopes {
		kind, gameID, _ := strings.Cut(key, "\x00")
		gameKey, link := saveVaultGameKey(kind, gameID, stored)
		audit := s.saveVaultScopeAuditLocked(kind, gameID, stored)
		member := saveVaultGameMember{
			Type: kind, GameID: gameID, Link: link, Head: audit.Head,
			SnapshotCount: audit.SnapshotCount, ConflictCount: audit.ConflictCount,
			ArchiveBytes: audit.ArchiveBytes, LastReceivedAt: audit.LastReceivedAt,
		}
		if tombstone, ok := s.saveVaultTombstone(kind, gameID); ok && tombstone.State == saveVaultTombstoneRetired {
			member.Retired = true
		}
		if stored.Identity != nil {
			member.Name = stored.Identity.Name
		}
		game := byKey[gameKey]
		if game == nil {
			game = &saveVaultGame{Key: gameKey, Type: kind}
			byKey[gameKey] = game
		}
		game.Scopes = append(game.Scopes, member)
		if member.LastReceivedAt.After(game.LastReceivedAt) {
			game.LastReceivedAt = member.LastReceivedAt
		}
	}
	games := make([]saveVaultGame, 0, len(byKey))
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	for _, game := range byKey {
		sort.Slice(game.Scopes, func(i, j int) bool {
			if !game.Scopes[i].LastReceivedAt.Equal(game.Scopes[j].LastReceivedAt) {
				return game.Scopes[i].LastReceivedAt.After(game.Scopes[j].LastReceivedAt)
			}
			return game.Scopes[i].GameID < game.Scopes[j].GameID
		})
		game.Name = saveVaultGameName(game.Scopes)
		game.Meta = s.saveVaultGameMetaLocked(game.Key, game.Scopes, admin)
		games = append(games, *game)
	}
	sort.Slice(games, func(i, j int) bool {
		if !games[i].LastReceivedAt.Equal(games[j].LastReceivedAt) {
			return games[i].LastReceivedAt.After(games[j].LastReceivedAt)
		}
		return games[i].Key < games[j].Key
	})
	return games
}

// saveVaultGameClientName returns the name reported by the most recently
// active member that does not rely on a hand-made link. A game made only of
// hand-linked members falls back to their names too.
func saveVaultGameClientName(members []saveVaultGameMember) (string, bool) {
	for _, member := range members {
		if member.Link != gameLinkManual && member.Name != "" {
			return member.Name, true
		}
	}
	for _, member := range members {
		if member.Link != gameLinkManual {
			return "", false
		}
	}
	for _, member := range members {
		if member.Name != "" {
			return member.Name, true
		}
	}
	return "", false
}

// saveVaultGameName names a game after the members that belong to it on
// their own: the name the most recently active of them reported, else its
// scope ID. Only a game made of hand-linked members alone takes their name.
func saveVaultGameName(members []saveVaultGameMember) string {
	if name, ok := saveVaultGameClientName(members); ok {
		return name
	}
	for _, member := range members {
		if member.Link != gameLinkManual {
			return member.GameID
		}
	}
	return members[0].GameID
}

// saveVaultGameMembersForKeyLocked collects the members of one game key in the
// same order listSaveVaultGamesLocked uses. The caller holds saveVaultMu.
func (s *server) saveVaultGameMembersForKeyLocked(gameKey string) []saveVaultGameMember {
	members := make([]saveVaultGameMember, 0)
	for key, stored := range s.saveVaultState.Scopes {
		kind, gameID, _ := strings.Cut(key, "\x00")
		candidate, link := saveVaultGameKey(kind, gameID, stored)
		if candidate != gameKey {
			continue
		}
		member := saveVaultGameMember{Type: kind, GameID: gameID, Link: link}
		if tombstone, ok := s.saveVaultTombstone(kind, gameID); ok && tombstone.State == saveVaultTombstoneRetired {
			member.Retired = true
		}
		if stored.Identity != nil {
			member.Name = stored.Identity.Name
		}
		for _, item := range stored.Snapshots {
			if item.ReceivedAt.After(member.LastReceivedAt) {
				member.LastReceivedAt = item.ReceivedAt
			}
		}
		members = append(members, member)
	}
	sort.Slice(members, func(i, j int) bool {
		if !members[i].LastReceivedAt.Equal(members[j].LastReceivedAt) {
			return members[i].LastReceivedAt.After(members[j].LastReceivedAt)
		}
		return members[i].GameID < members[j].GameID
	})
	return members
}

func (s *server) handleListSaveVaultGames(w http.ResponseWriter, _ *http.Request) {
	s.saveVaultMu.Lock()
	games := s.listSaveVaultGamesLocked()
	s.saveVaultMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"games": games})
}

func (s *server) handleAdminListSaveVaultGames(w http.ResponseWriter, _ *http.Request) {
	s.saveVaultMu.Lock()
	games := s.listSaveVaultGamesForAdminLocked(true)
	s.saveVaultMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"games": games})
}

func (s *server) handleGetSaveVaultGame(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	s.saveVaultMu.Lock()
	games := s.listSaveVaultGamesLocked()
	s.saveVaultMu.Unlock()
	for _, game := range games {
		if game.Key == key {
			writeJSON(w, http.StatusOK, game)
			return
		}
	}
	writeError(w, http.StatusNotFound, "SaveVault game not found")
}

// handlePutSaveVaultScopeIdentity records what a client knows about a scope's
// game. It never touches a head or a snapshot and writes nothing when the
// facts are unchanged, so clients may send it after every backup.
func (s *server) handlePutSaveVaultScopeIdentity(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	var identity saveVaultScopeIdentity
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		writeError(w, http.StatusBadRequest, "invalid SaveVault game identity")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault game identity")
		return
	}
	identity, ok = normalizeScopeIdentity(identity)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid SaveVault game identity fields")
		return
	}

	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	stored, exists := s.saveVaultState.Scopes[key]
	if exists && stored.Identity != nil && stored.Identity.sameFacts(identity) {
		gameKey, link := saveVaultGameKey(kind, gameID, stored)
		s.queueSaveVaultCoverFetch(gameKey)
		writeJSON(w, http.StatusOK, map[string]string{"game": gameKey, "link": link})
		return
	}
	nextScope := stored.clone()
	identity.UpdatedAt = time.Now().UTC()
	nextScope.Identity = &identity
	nextState := s.saveVaultState.clone()
	nextState.Scopes[key] = nextScope
	if err := s.persistSaveVaultState(nextState); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault scope state")
		return
	}
	s.saveVaultState = nextState
	gameKey, link := saveVaultGameKey(kind, gameID, nextScope)
	s.queueSaveVaultCoverFetch(gameKey)
	writeJSON(w, http.StatusOK, map[string]string{"game": gameKey, "link": link})
}

// handlePutSaveVaultScopeGame links a scope to a game by hand, or with an
// empty key returns it to the automatic grouping. Only an existing scope can
// be linked, and only to a key of its own type.
func (s *server) handlePutSaveVaultScopeGame(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	var request struct {
		Game string `json:"game"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid game link")
		return
	}
	request.Game = strings.TrimSpace(request.Game)
	if request.Game != "" && !validManualGameKey(kind, request.Game) {
		writeError(w, http.StatusBadRequest, "a game link must be a key of the scope's own type")
		return
	}

	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		if tombstone.State == saveVaultTombstoneRetired {
			writeError(w, http.StatusConflict, "retired SaveVault scope is read-only; unretire it first")
		} else {
			writeError(w, http.StatusConflict, "deleted SaveVault scope cannot be linked")
		}
		return
	}
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists {
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	nextScope := stored.clone()
	nextScope.GameLink = request.Game
	nextState := s.saveVaultState.clone()
	nextState.Scopes[key] = nextScope
	if err := s.persistSaveVaultState(nextState); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault scope state")
		return
	}
	s.saveVaultState = nextState
	gameKey, link := saveVaultGameKey(kind, gameID, nextScope)
	s.queueSaveVaultCoverFetch(gameKey)
	writeJSON(w, http.StatusOK, map[string]string{"game": gameKey, "link": link})
}
