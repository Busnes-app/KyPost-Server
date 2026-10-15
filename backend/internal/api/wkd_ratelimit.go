package api

import (
	"math"
	"net/http"
	"strconv"
)

// wkdRateBurst/wkdRateRefillPerSec throttle the public, unauthenticated Web
// Key Directory endpoint per client IP. Unlike failureLockout (which counts
// *failed* attempts and is right for auth surfaces), every WKD request looks
// legitimate — there is no failure to strike on — so this is a plain
// request-rate token bucket instead.
//
// The endpoint is cheap for an unknown domain (one instance-store read, then
// 404) but a match costs a per-user scan that decrypts the IMAP config to
// resolve publishable addresses. Nothing about the route is authenticated, so
// without a limit one host can drive that work in a loop.
//
// A burst of 30 with 1 token/sec sustained is far above real usage — a mail
// client looks a correspondent's key up once and caches it, and even a busy
// MTA batching lookups stays well under this — while capping a single abusive
// client at 60 requests/minute.
const (
	wkdRateBurst        = 30
	wkdRateRefillPerSec = 1.0
)

// withWKDRateLimit throttles the public WKD endpoint per client IP, answering
// 429 with Retry-After once a client exceeds its budget.
func (s *Server) withWKDRateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.wkdLimiter != nil {
			if ok, retryAfter := s.wkdLimiter.allow(lockoutKeyForIP(clientIP(r))); !ok {
				seconds := int(math.Ceil(retryAfter.Seconds()))
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
		}
		next(w, r)
	}
}

// pushPollBurst/pushPollRefillPerSec meter the two unauthenticated push-MFA
// endpoints PER IP, in requests.
//
// /api/auth/mfa/push/poll and /api/auth/mfa/push/finish take a challenge id and
// nothing else, and they were the only public routes in the table with no meter
// at all. The challenge id is 24 bytes from crypto/rand, so this is not about
// guessing it — it is that both take mfa.Store's single process-wide mutex, and
// LoginPage polls at 1.5s intervals, so an anonymous caller could contend that
// lock as fast as it could open connections, against every sign-in in progress.
//
// Generous, because a real browser legitimately polls every 1.5s for the whole
// five-minute challenge TTL: the refill sustains that with room to spare, and
// the burst covers a tab that wakes up and catches up.
const (
	pushPollBurst        = 60
	pushPollRefillPerSec = 2
)

// withPushPollRateLimit throttles the public push-challenge endpoints per client
// IP. Same shape as withWKDRateLimit, and it shares that limiter's key function
// so an IPv6 caller is metered on its /64 rather than per address.
func (s *Server) withPushPollRateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.pushPollLimiter != nil {
			if ok, retryAfter := s.pushPollLimiter.allow(lockoutKeyForIP(clientIP(r))); !ok {
				seconds := int(math.Ceil(retryAfter.Seconds()))
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many requests"})
				return
			}
		}
		next(w, r)
	}
}

// accountWriteBurst/accountWriteRefillPerSec meter MUTATING withAuth requests
// per account.
//
// The bound being defended is architectural, not per-endpoint: users.Store
// rewrites users.json WHOLE, under a process mutex and a cross-process file
// lock, on every account mutation — and every authenticated request reads that
// same file through currentUser. Measured on real disk, one session looping
// POST /api/mfa/totp/setup drove 335 whole-file rewrites/s and took a DIFFERENT
// user's Get() from 1.58M/s to 6,770/s at 1 KiB, and to 11/s at 4 MiB.
//
// Eliding no-op writes (errNoChangeNeeded) is the cheaper fix where it applies,
// and it is applied. It cannot apply to a route like TOTP setup, which mints a
// fresh secret per call and so genuinely changes the file every time. That is
// why this exists at the wrapper rather than at the endpoints: the next such
// route gets the bound without anyone remembering to ask for it.
//
// Sized well above human use and far below abuse. A burst of 90 with 10/s
// sustained covers any real session — the SPA's heaviest sequence is a handful
// of writes behind one click — while cutting the measured attack by ~33x. It
// meters only non-GET requests, so reading is untouched.
const (
	accountWriteBurst        = 90
	accountWriteRefillPerSec = 10.0
)
