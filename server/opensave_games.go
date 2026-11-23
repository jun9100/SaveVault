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

// Games group SaveVault (OpenSave) scopes of one type that hold saves of the
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

// openSaveScopeIdentity is what a client knows about the game behind a scope.
type openSaveScopeIdentity struct {
	Name           string    `json:"name,omitempty"`
	TitleID        string    `json:"titleId,omitempty"`
	SteamAppID     string    `json:"steamAppId,omitempty"`
	PSPGameID      string    `json:"pspGameId,omitempty"`
	AndroidPackage string    `json:"androidPackage,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt,omitzero"`
}

func (identity openSaveScopeIdentity) sameFacts(other openSaveScopeIdentity) bool {
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

func normalizeScopeIdentity(identity openSaveScopeIdentity) (openSaveScopeIdentity, bool) {
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

// openSaveGameKey is the game a scope belongs to: a hand-made link first,
// then the strongest identity fact, then a Switch scope named after its
// TitleID, and otherwise the scope alone. Every key starts with the scope's
// type, so games never span save types.
func openSaveGameKey(kind, gameID string, stored openSaveScopeState) (string, string) {
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

type openSaveGameMember struct {
	Type           string                 `json:"type"`
	GameID         string                 `json:"gameId"`
	Link           string                 `json:"link"`
	Name           string                 `json:"name,omitempty"`
	Head           *openSaveScopeSnapshot `json:"head"`
	SnapshotCount  int                    `json:"snapshotCount"`
	ConflictCount  int                    `json:"conflictCount"`
	LastReceivedAt time.Time              `json:"lastReceivedAt,omitzero"`
	Retired        bool                   `json:"retired,omitempty"`
}

type openSaveGame struct {
	Key            string               `json:"key"`
	Type           string               `json:"type"`
	Name           string               `json:"name"`
	Meta           openSaveGameMeta     `json:"meta"`
	Scopes         []openSaveGameMember `json:"scopes"`
	LastReceivedAt time.Time            `json:"lastReceivedAt,omitzero"`
}

// listOpenSaveGamesLocked groups every scope into games, the most recently
// active first. The caller holds openSaveMu.
func (s *server) listOpenSaveGamesLocked() []openSaveGame {
	return s.listOpenSaveGamesForAdminLocked(false)
}

// listOpenSaveGamesForAdminLocked is listOpenSaveGamesLocked with the cover
// URL namespace selected by admin. The caller holds openSaveMu; this function
// acquires metaMu after it, following the documented openSaveMu -> metaMu
// order.
func (s *server) listOpenSaveGamesForAdminLocked(admin bool) []openSaveGame {
	byKey := make(map[string]*openSaveGame)
	for key, stored := range s.openSaveState.Scopes {
		kind, gameID, _ := strings.Cut(key, "\x00")
		gameKey, link := openSaveGameKey(kind, gameID, stored)
		audit := s.openSaveScopeAuditLocked(kind, gameID, stored)
		member := openSaveGameMember{
			Type: kind, GameID: gameID, Link: link, Head: audit.Head,
			SnapshotCount: audit.SnapshotCount, ConflictCount: audit.ConflictCount, LastReceivedAt: audit.LastReceivedAt,
		}
		if tombstone, ok := s.openSaveTombstone(kind, gameID); ok && tombstone.State == openSaveTombstoneRetired {
			member.Retired = true
		}
		if stored.Identity != nil {
			member.Name = stored.Identity.Name
		}
		game := byKey[gameKey]
		if game == nil {
			game = &openSaveGame{Key: gameKey, Type: kind}
			byKey[gameKey] = game
		}
		game.Scopes = append(game.Scopes, member)
		if member.LastReceivedAt.After(game.LastReceivedAt) {
			game.LastReceivedAt = member.LastReceivedAt
		}
	}
	games := make([]openSaveGame, 0, len(byKey))
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	for _, game := range byKey {
		sort.Slice(game.Scopes, func(i, j int) bool {
			if !game.Scopes[i].LastReceivedAt.Equal(game.Scopes[j].LastReceivedAt) {
				return game.Scopes[i].LastReceivedAt.After(game.Scopes[j].LastReceivedAt)
			}
			return game.Scopes[i].GameID < game.Scopes[j].GameID
		})
		game.Name = openSaveGameName(game.Scopes)
		game.Meta = s.openSaveGameMetaLocked(game.Key, game.Scopes, admin)
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

// openSaveGameClientName returns the name reported by the most recently
// active member that does not rely on a hand-made link. A game made only of
// hand-linked members falls back to their names too.
func openSaveGameClientName(members []openSaveGameMember) (string, bool) {
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

// openSaveGameName names a game after the members that belong to it on
// their own: the name the most recently active of them reported, else its
// scope ID. Only a game made of hand-linked members alone takes their name.
func openSaveGameName(members []openSaveGameMember) string {
	if name, ok := openSaveGameClientName(members); ok {
		return name
	}
	for _, member := range members {
		if member.Link != gameLinkManual {
			return member.GameID
		}
	}
	return members[0].GameID
}

// openSaveGameMembersForKeyLocked collects the members of one game key in the
// same order listOpenSaveGamesLocked uses. The caller holds openSaveMu.
func (s *server) openSaveGameMembersForKeyLocked(gameKey string) []openSaveGameMember {
	members := make([]openSaveGameMember, 0)
	for key, stored := range s.openSaveState.Scopes {
		kind, gameID, _ := strings.Cut(key, "\x00")
		candidate, link := openSaveGameKey(kind, gameID, stored)
		if candidate != gameKey {
			continue
		}
		member := openSaveGameMember{Type: kind, GameID: gameID, Link: link}
		if tombstone, ok := s.openSaveTombstone(kind, gameID); ok && tombstone.State == openSaveTombstoneRetired {
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

func (s *server) handleListOpenSaveGames(w http.ResponseWriter, _ *http.Request) {
	s.openSaveMu.Lock()
	games := s.listOpenSaveGamesLocked()
	s.openSaveMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"games": games})
}

func (s *server) handleAdminListOpenSaveGames(w http.ResponseWriter, _ *http.Request) {
	s.openSaveMu.Lock()
	games := s.listOpenSaveGamesForAdminLocked(true)
	s.openSaveMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"games": games})
}

func (s *server) handleGetOpenSaveGame(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	s.openSaveMu.Lock()
	games := s.listOpenSaveGamesLocked()
	s.openSaveMu.Unlock()
	for _, game := range games {
		if game.Key == key {
			writeJSON(w, http.StatusOK, game)
			return
		}
	}
	writeError(w, http.StatusNotFound, "OpenSave game not found")
}

// handlePutOpenSaveScopeIdentity records what a client knows about a scope's
// game. It never touches a head or a snapshot and writes nothing when the
// facts are unchanged, so clients may send it after every backup.
func (s *server) handlePutOpenSaveScopeIdentity(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.openSaveScopeBlocked(kind, gameID); blocked {
		writeOpenSaveTombstone(w, tombstone)
		return
	}
	var identity openSaveScopeIdentity
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		writeError(w, http.StatusBadRequest, "invalid OpenSave game identity")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave game identity")
		return
	}
	identity, ok = normalizeScopeIdentity(identity)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid OpenSave game identity fields")
		return
	}

	s.openSaveMu.Lock()
	defer s.openSaveMu.Unlock()
	if tombstone, blocked := s.openSaveScopeBlockedLocked(kind, gameID); blocked {
		writeOpenSaveTombstone(w, tombstone)
		return
	}
	stored, exists := s.openSaveState.Scopes[key]
	if exists && stored.Identity != nil && stored.Identity.sameFacts(identity) {
		gameKey, link := openSaveGameKey(kind, gameID, stored)
		s.queueOpenSaveCoverFetch(gameKey)
		writeJSON(w, http.StatusOK, map[string]string{"game": gameKey, "link": link})
		return
	}
	nextScope := stored.clone()
	identity.UpdatedAt = time.Now().UTC()
	nextScope.Identity = &identity
	nextState := s.openSaveState.clone()
	nextState.Scopes[key] = nextScope
	if err := s.persistOpenSaveState(nextState); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave scope state")
		return
	}
	s.openSaveState = nextState
	gameKey, link := openSaveGameKey(kind, gameID, nextScope)
	s.queueOpenSaveCoverFetch(gameKey)
	writeJSON(w, http.StatusOK, map[string]string{"game": gameKey, "link": link})
}

// handlePutOpenSaveScopeGame links a scope to a game by hand, or with an
// empty key returns it to the automatic grouping. Only an existing scope can
// be linked, and only to a key of its own type.
func (s *server) handlePutOpenSaveScopeGame(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
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

	s.openSaveMu.Lock()
	defer s.openSaveMu.Unlock()
	if tombstone, blocked := s.openSaveScopeBlockedLocked(kind, gameID); blocked {
		if tombstone.State == openSaveTombstoneRetired {
			writeError(w, http.StatusConflict, "retired OpenSave scope is read-only; unretire it first")
		} else {
			writeError(w, http.StatusConflict, "deleted OpenSave scope cannot be linked")
		}
		return
	}
	stored, exists := s.openSaveState.Scopes[key]
	if !exists {
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	nextScope := stored.clone()
	nextScope.GameLink = request.Game
	nextState := s.openSaveState.clone()
	nextState.Scopes[key] = nextScope
	if err := s.persistOpenSaveState(nextState); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave scope state")
		return
	}
	s.openSaveState = nextState
	gameKey, link := openSaveGameKey(kind, gameID, nextScope)
	s.queueOpenSaveCoverFetch(gameKey)
	writeJSON(w, http.StatusOK, map[string]string{"game": gameKey, "link": link})
}
