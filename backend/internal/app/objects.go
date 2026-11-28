package app

import (
	"context"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// probeObjectStorage refuses a multi start whose bucket refuses the credentials or does not
// refuse an existing key on a conditional create (MH11), before any store opens.
func probeObjectStorage(cells []string) error {
	c, err := config.ObjectStorage()
	if err != nil {
		return err
	}
	o, err := vol.NewObject(c, cells, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return o.Probe(ctx)
}
