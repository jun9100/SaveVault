package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func publishOpenSaveScope(t *testing.T, baseURL, kind, gameID, snapshotID, parent string, body []byte) {
	t.Helper()
	archive := gameID + "__main__" + snapshotID + ".zip"
	if status := putOpenSaveScopeArchive(t, baseURL, kind, archive, body); status != http.StatusCreated {
		t.Fatalf("archive %s status = %d", archive, status)
	}
	candidate := openSaveScopeCandidateFor(snapshotID, archive, parent, body, time.Now().UTC())
	response, raw := putOpenSaveScopeCandidate(t, baseURL,
		"/v1/opensave/scopes/"+kind+"/"+gameID+"/snapshots/"+snapshotID, parent, candidate)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("publish %s/%s status = %d: %s", kind, gameID, response.StatusCode, raw)
	}
}

func putOpenSaveIdentity(t *testing.T, baseURL, kind, gameID string, identity map[string]string) (int, map[string]string) {
	t.Helper()
	payload, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	request := openSaveScopeRequest(t, http.MethodPut, baseURL+"/v1/opensave/scopes/"+kind+"/"+gameID+"/identity", payload)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(response.Body).Decode(&body)
	return response.StatusCode, body
}

func listOpenSaveGames(t *testing.T, baseURL string) []openSaveGame {
	t.Helper()
	request := openSaveScopeRequest(t, http.MethodGet, baseURL+"/v1/opensave/games", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("games status = %d", response.StatusCode)
	}
	var body struct {
		Games []openSaveGame `json:"games"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Games
}

func gameByKey(games []openSaveGame, key string) *openSaveGame {
	for i := range games {
		if games[i].Key == key {
			return &games[i]
		}
	}
	return nil
}

func TestOpenSaveGamesGroupScopesOfOneTitleWithinOneType(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	// A PC names its Switch scope after the TitleID; Android hashes its own.
	publishOpenSaveScope(t, httpServer.URL, "switch", "switch-0100000000010000", "pc-1", "", []byte("pc odyssey"))
	publishOpenSaveScope(t, httpServer.URL, "switch", "android-switch-0123456789abcdef", "phone-1", "", []byte("phone odyssey"))
	if status, body := putOpenSaveIdentity(t, httpServer.URL, "switch", "android-switch-0123456789abcdef",
		map[string]string{"name": "超级马力欧 奥德赛", "titleId": "0100000000010000"}); status != http.StatusOK ||
		body["game"] != "switch/title:0100000000010000" || body["link"] != gameLinkAuto {
		t.Fatalf("identity = %d %v", status, body)
	}
	// The same Steam AppID under two types stays two games.
	publishOpenSaveScope(t, httpServer.URL, "pc", "elden-ring", "pc-1", "", []byte("pc elden"))
	publishOpenSaveScope(t, httpServer.URL, "emulator-winlator", "elden-ring", "w-1", "", []byte("winlator elden"))
	putOpenSaveIdentity(t, httpServer.URL, "pc", "elden-ring", map[string]string{"steamAppId": "1245620"})
	putOpenSaveIdentity(t, httpServer.URL, "emulator-winlator", "elden-ring", map[string]string{"steamAppId": "1245620"})

	games := listOpenSaveGames(t, httpServer.URL)
	odyssey := gameByKey(games, "switch/title:0100000000010000")
	if odyssey == nil || len(odyssey.Scopes) != 2 || odyssey.Name != "超级马力欧 奥德赛" {
		t.Fatalf("odyssey game = %+v", odyssey)
	}
	links := map[string]string{}
	for _, member := range odyssey.Scopes {
		links[member.GameID] = member.Link
		if member.Head == nil || member.SnapshotCount != 1 {
			t.Fatalf("member = %+v", member)
		}
	}
	if links["switch-0100000000010000"] != gameLinkAuto || links["android-switch-0123456789abcdef"] != gameLinkAuto {
		t.Fatalf("links = %v", links)
	}
	if pc, winlator := gameByKey(games, "pc/steam:1245620"), gameByKey(games, "emulator-winlator/steam:1245620"); pc == nil || winlator == nil ||
		len(pc.Scopes) != 1 || len(winlator.Scopes) != 1 {
		t.Fatalf("steam games = %+v / %+v", pc, winlator)
	}

	// The single-game route takes the key with its slash.
	request := openSaveScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/opensave/games/"+url.PathEscape("switch/title:0100000000010000"), nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("single game status = %d", response.StatusCode)
	}
}

func TestOpenSaveIdentityNeverMovesAHeadAndSurvivesLaterUploads(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "android-pc-00000000000000aa", "a", "", []byte("A"))
	putOpenSaveIdentity(t, httpServer.URL, "pc", "android-pc-00000000000000aa",
		map[string]string{"name": "Half-Life", "steamAppId": "70"})

	key := openSaveScopeKey("pc", "android-pc-00000000000000aa")
	s.openSaveMu.Lock()
	stored := s.openSaveState.Scopes[key]
	firstUpdate := stored.Identity.UpdatedAt
	s.openSaveMu.Unlock()
	if stored.Head != "a" || len(stored.Snapshots) != 1 {
		t.Fatalf("identity changed the scope: %+v", stored)
	}

	// Unchanged facts write nothing.
	time.Sleep(5 * time.Millisecond)
	putOpenSaveIdentity(t, httpServer.URL, "pc", "android-pc-00000000000000aa",
		map[string]string{"name": "Half-Life", "steamAppId": "70"})
	// A later upload keeps the identity (scope clones must copy it).
	publishOpenSaveScope(t, httpServer.URL, "pc", "android-pc-00000000000000aa", "b", "a", []byte("B"))

	s.openSaveMu.Lock()
	stored = s.openSaveState.Scopes[key]
	s.openSaveMu.Unlock()
	if stored.Head != "b" || stored.Identity == nil || stored.Identity.SteamAppID != "70" || !stored.Identity.UpdatedAt.Equal(firstUpdate) {
		t.Fatalf("after upload = %+v identity = %+v", stored, stored.Identity)
	}

	// The identity survives a restart.
	reloaded, err := newServer(s.dataDir, testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	if identity := reloaded.openSaveState.Scopes[key].Identity; identity == nil || identity.Name != "Half-Life" {
		t.Fatalf("reloaded identity = %+v", identity)
	}
}

func TestOpenSaveIdentityRejectsInvalidFacts(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	for _, identity := range []map[string]string{
		{"titleId": "not-a-title"},
		{"steamAppId": "12ab"},
		{"pspGameId": "ULUS1004"},
		{"androidPackage": "no-dots"},
		{"name": "bad\nname"},
		{"unexpected": "field"},
	} {
		if status, _ := putOpenSaveIdentity(t, httpServer.URL, "pc", "some-game", identity); status != http.StatusBadRequest {
			t.Fatalf("identity %v status = %d", identity, status)
		}
	}
}

func TestOpenSaveGamesCanBeLinkedByHandWithinOneType(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "dawn-of-war-2", "a", "", []byte("pc"))
	publishOpenSaveScope(t, httpServer.URL, "pc", "android-pc-00000000000000bb", "a", "", []byte("phone"))

	link := func(gameID, game string) (int, map[string]string) {
		t.Helper()
		response, raw := adminDo(t, http.MethodPut, httpServer.URL+"/admin/api/opensave/scopes/pc/"+gameID+"/game", map[string]string{"game": game})
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		return response.StatusCode, body
	}
	putOpenSaveIdentity(t, httpServer.URL, "pc", "android-pc-00000000000000bb", map[string]string{"name": "Some Other Name"})
	if status, body := link("android-pc-00000000000000bb", "pc/scope:dawn-of-war-2"); status != http.StatusOK || body["link"] != gameLinkManual {
		t.Fatalf("link = %d %v", status, body)
	}
	games := listOpenSaveGames(t, httpServer.URL)
	if game := gameByKey(games, "pc/scope:dawn-of-war-2"); game == nil || len(game.Scopes) != 2 || game.Name != "dawn-of-war-2" {
		t.Fatalf("linked game = %+v", game)
	}
	// Other types, malformed keys and unknown scopes are refused.
	if status, _ := link("android-pc-00000000000000bb", "switch/title:0100000000010000"); status != http.StatusBadRequest {
		t.Fatalf("cross-type link status = %d", status)
	}
	if status, _ := link("android-pc-00000000000000bb", "pc/has space"); status != http.StatusBadRequest {
		t.Fatalf("malformed link status = %d", status)
	}
	if status, _ := link("no-such-scope", "pc/scope:dawn-of-war-2"); status != http.StatusNotFound {
		t.Fatalf("unknown scope status = %d", status)
	}
	// An empty key returns the scope to automatic grouping.
	if status, body := link("android-pc-00000000000000bb", ""); status != http.StatusOK || body["game"] != "pc/scope:android-pc-00000000000000bb" {
		t.Fatalf("unlink = %d %v", status, body)
	}
	// Linking needs the admin write header.
	request, _ := http.NewRequest(http.MethodPut, httpServer.URL+"/admin/api/opensave/scopes/pc/dawn-of-war-2/game", nil)
	request.SetBasicAuth("admin", testAdminToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("without write header status = %d", response.StatusCode)
	}
}
