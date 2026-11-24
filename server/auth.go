// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/subtle"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookie   = "game_save_session"
	sessionLifetime = 7 * 24 * time.Hour
	// Mutating dashboard requests must carry this header. Browsers cannot attach custom headers to
	// cross-site requests without a CORS preflight, which this service never grants.
	csrfHeader       = "X-Game-Save-Admin"
	legacyCSRFHeader = "X-Eden-Admin"

	loginFailureLimit      = 8
	loginFailureWindow     = 10 * time.Minute
	maxLoginFailureEntries = 4096
)

type loginFailures struct {
	count int
	start time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]loginFailures
}

func (l *loginLimiter) blocked(address string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.failures[address]
	if !ok {
		return false
	}
	if now.Sub(entry.start) > loginFailureWindow {
		delete(l.failures, address)
		return false
	}
	return entry.count >= loginFailureLimit
}

func (l *loginLimiter) fail(address string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures == nil {
		l.failures = make(map[string]loginFailures)
	}
	for key, entry := range l.failures {
		if now.Sub(entry.start) > loginFailureWindow {
			delete(l.failures, key)
		}
	}
	entry, ok := l.failures[address]
	if !ok {
		if len(l.failures) >= maxLoginFailureEntries {
			var oldestKey string
			var oldestTime time.Time
			for key, existing := range l.failures {
				if oldestKey == "" || existing.start.Before(oldestTime) {
					oldestKey = key
					oldestTime = existing.start
				}
			}
			if oldestKey != "" {
				delete(l.failures, oldestKey)
			}
		}
		entry.start = now
	}
	entry.count++
	l.failures[address] = entry
	return entry.count == loginFailureLimit
}

func (l *loginLimiter) reset(address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, address)
}

func (s *server) validSession(value string, now time.Time) bool {
	_, ok := s.sessions.validate(value, sessionRoleAdmin, now)
	return ok
}

func (s *server) adminTokenMatches(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.adminToken)) == 1
}

// adminAuthenticated accepts either the dashboard session cookie or HTTP Basic Auth whose
// password is the administrator token (useful for scripts and Prometheus).
// Basic Auth attempts share the per-IP rate limiter with /admin/api/login.
func (s *server) adminAuthenticated(r *http.Request) bool {
	ok, _ := s.adminAuthenticatedWithLimit(r, true)
	return ok
}

// adminAuthenticatedPeek checks session cookie or Basic Auth without recording
// a failed attempt, used during routing before requireAdmin enforces limits.
func (s *server) adminAuthenticatedPeek(r *http.Request) bool {
	ok, _ := s.adminAuthenticatedWithLimit(r, false)
	return ok
}

func (s *server) adminAuthenticatedWithLimit(r *http.Request, recordFailure bool) (bool, bool) {
	now := s.now()
	if cookie, err := r.Cookie(sessionCookie); err == nil && s.validSession(cookie.Value, now) {
		return true, false
	}
	if _, password, ok := r.BasicAuth(); ok {
		address := s.clientIP(r)
		if s.logins.blocked(address, now) {
			return false, true
		}
		if s.adminTokenMatches(password) {
			s.logins.reset(address)
			return true, false
		}
		if recordFailure && s.logins.fail(address, now) {
			s.notifyWebhooks(event{Time: now.UTC(), Kind: eventAuthLocked, Detail: "admin basic auth locked for " + address})
		}
		return false, false
	}
	return false, false
}

func (s *server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" {
			http.NotFound(w, r)
			return
		}
		ok, blocked := s.adminAuthenticatedWithLimit(r, true)
		if blocked {
			writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
			return
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "administrator authentication required")
			return
		}
		next(w, r)
	}
}

// requireAdminWrite additionally requires the anti-CSRF header for state-changing requests.
func (s *server) requireAdminWrite(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if !hasAdminWriteHeader(r) {
			writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		next(w, r)
	})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.adminToken == "" {
		http.NotFound(w, r)
		return
	}
	if !hasAdminWriteHeader(r) {
		writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
		return
	}
	now := s.now()
	address := s.clientIP(r)
	if s.logins.blocked(address, now) {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	var request struct {
		Token string `json:"token"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	if !s.adminTokenMatches(request.Token) {
		if s.logins.fail(address, now) {
			s.notifyWebhooks(event{Time: now.UTC(), Kind: eventAuthLocked, Detail: "admin login locked for " + address})
		}
		writeError(w, http.StatusUnauthorized, "invalid administrator token")
		return
	}
	s.logins.reset(address)
	value, rec, err := s.sessions.create(sessionRoleAdmin, "", defaultVaultID, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot allocate session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  rec.ExpiresAt,
		HttpOnly: true,
		Secure:   s.trustedProxies.isSecure(r),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "expires_at": rec.ExpiresAt.UTC()})
}

func hasAdminWriteHeader(r *http.Request) bool {
	return r.Header.Get(csrfHeader) == "1" || r.Header.Get(legacyCSRFHeader) == "1"
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !hasAdminWriteHeader(r) {
		writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.revokeCookie(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.trustedProxies.isSecure(r),
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	if s.adminToken == "" {
		http.NotFound(w, r)
		return
	}
	authenticated, blocked := s.adminAuthenticatedWithLimit(r, true)
	if blocked {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": authenticated, "version": version})
}
