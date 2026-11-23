package api

import (
	"errors"
	"net/http"

	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// A bounded document retry is a temporary contention refusal. Every other error
// retains the handler's existing status and message, including in file mode.
func documentStatus(err error, fallback int) int {
	if errors.Is(err, vol.ErrBusy) || errors.Is(err, vol.ErrStale) {
		return http.StatusServiceUnavailable
	}
	return fallback
}
