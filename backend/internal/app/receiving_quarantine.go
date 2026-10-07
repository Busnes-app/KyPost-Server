package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

const quarantineUsage = "usage: receiving quarantine list [<after-sequence>] | receiving quarantine release|discard <gateway> <id> --confirm <id>"

// runReceivingQuarantine is the operator CLI for quarantined deliveries.
// Shell access as the runtime user already holds every key the admin API's
// step-up guards, so the CLI asks for deliberate intent instead: the
// delivery ID typed again after --confirm. Envelope output only. Its audit
// line goes to the invoking terminal; the tombstone disposition and mailbox
// receipts are the durable record.
func runReceivingQuarantine(args []string, output io.Writer) (result error) {
	actor := "cli:" + strconv.Itoa(os.Geteuid())
	action, target, status := "list", "holding-store", "committed"
	defer func() {
		if result != nil {
			status = "refused"
		}
		slog.Info("receiving quarantine operation", "actor", actor, "task_id", "native-receiving", "action", action, "target", target, "result", status, "correlation_id", target)
	}()
	if len(args) > 0 {
		action = args[0]
	}
	var after int64
	switch {
	case len(args) == 5 && (args[0] == "release" || args[0] == "discard") && ingress.ValidIdentifier(args[1]) && ingress.ValidIdentifier(args[2]):
		target = args[1] + "/" + args[2]
		if args[3] != "--confirm" || args[4] != args[2] {
			return errors.New("refused: repeat the delivery ID after --confirm to " + args[0] + " it")
		}
	case len(args) >= 1 && len(args) <= 2 && args[0] == "list":
		if len(args) == 2 {
			var err error
			if after, err = strconv.ParseInt(args[1], 10, 64); err != nil || after < 0 {
				return errors.New(quarantineUsage)
			}
		}
	default:
		return errors.New(quarantineUsage)
	}
	// Another user would create files the services cannot open, or lack access.
	info, err := os.Stat(config.StateDir())
	if err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("run as the owner of %s: docker compose exec --user kypost kypost-server kypost-server receiving quarantine <command>", config.StateDir())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := openReceivingRuntime(ctx, false)
	if err != nil {
		return err
	}
	defer r.holding.Close()
	switch action {
	case "list":
		rows, err := r.life.QuarantinedDeliveries(ctx, r.holding, after)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]any{"deliveries": rows})
	case "release":
		status = "released"
		return r.life.ReleaseQuarantined(ctx, r.stateDir, sso.NewStore(r.configDir).Load().IssuerURL, r.accounts, r.holding, args[1], args[2])
	default:
		status, err = r.holding.Discard(ctx, args[1], args[2])
		if err == nil {
			_, err = fmt.Fprintln(output, status)
		}
		return err
	}
}
