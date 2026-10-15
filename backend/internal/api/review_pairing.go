package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// handleReviewPairing is an explicit Play-review escape hatch for the one
// account named by REVIEW_PAIRING_USERNAME (exact match). It takes the same
// derived credential and the same protections as handleLogin (verifyLogin);
// any other username fails like a wrong password.
func (s *Server) handleReviewPairing(w http.ResponseWriter, r *http.Request) {
	reviewUsername := users.NormalizeUsername(strings.TrimSpace(os.Getenv("REVIEW_PAIRING_USERNAME")))
	if reviewUsername == "" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Username     string `json:"username"`
		AuthSecret   string `json:"authSecret"`
		CaptchaToken string `json:"captchaToken,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	u, ok := s.verifyLogin(w, r, s.reviewPairingLockout, req.Username, req.AuthSecret, req.CaptchaToken, func(u users.User) bool {
		return users.NormalizeUsername(u.Username) == reviewUsername
	})
	if !ok {
		return
	}
	if u.MustChangePassword || u.TOTPEnabled || u.PushMFAEnabled {
		http.Error(w, "review account requires an interactive sign-in", http.StatusForbidden)
		return
	}
	s.writeNotificationPairing(w, u.ID)
}
