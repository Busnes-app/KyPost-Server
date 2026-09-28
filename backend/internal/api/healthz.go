package api

import (
	"context"
	"errors"
	"net/http"

	suitehealth "github.com/Busnes-app/ky-primitives/health"
)

// suiteHealthHandler reports the existing API and daemon health as one coarse check.
func (s *Server) suiteHealthHandler() http.Handler {
	return suitehealth.Handler("kypost", s.logger.Suite(), suitehealth.Check{
		Name: "service",
		Run: func(context.Context) error {
			if !s.mergedHealthStatus().Healthy {
				return errors.New("service unavailable")
			}
			return nil
		},
	})
}
