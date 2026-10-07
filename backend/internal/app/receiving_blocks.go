package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

const blocksUsage = "usage: receiving blocks list | receiving blocks add address|domain <value> [--until <RFC3339>] [--reason spam|phishing|abuse|other] --confirm <value> | receiving blocks remove address|domain <value> --confirm <value>"

// runReceivingBlocks is the operator CLI for manual sender blocks. Like
// quarantine, shell access needs deliberate intent rather than step-up: the
// value typed again after --confirm. Its audit line carries the block ID,
// never the address.
func runReceivingBlocks(args []string, output io.Writer) (result error) {
	actor := "cli:" + strconv.Itoa(os.Geteuid())
	action, target, id, status := "list", "sender-blocks", "", "committed"
	defer func() {
		if result != nil {
			status = "refused"
		}
		// The API's action names, so one audit vocabulary covers both.
		audit := map[string]string{"add": "block_sender", "remove": "unblock_sender"}[action]
		if audit == "" {
			audit = "list_sender_blocks"
		}
		slog.Info("receiving sender block change", "actor", actor, "task_id", "native-receiving", "action", audit, "target", target, "result", status, "correlation_id", id)
	}()
	if len(args) > 0 {
		action = args[0]
	}
	block := ingress.SenderBlock{Source: "manual", Actor: actor}
	switch {
	case len(args) == 1 && args[0] == "list":
	case len(args) >= 5 && (args[0] == "add" || args[0] == "remove") && (args[1] == "address" || args[1] == "domain") && len(args)%2 == 1:
		block.Kind, block.Value, target = args[1], args[2], args[1]
		confirmed := false
		for i := 3; i < len(args); i += 2 {
			switch flag, v := args[i], args[i+1]; {
			case flag == "--confirm" && !confirmed:
				if v != args[2] {
					return errors.New("refused: repeat the value after --confirm to " + args[0] + " the block")
				}
				confirmed = true
			case flag == "--until" && args[0] == "add" && block.Until == nil:
				at, err := time.Parse(time.RFC3339, v)
				if err != nil {
					return errors.New("--until takes an RFC3339 time, for example 2026-12-31T00:00:00Z")
				}
				ms := at.UnixMilli()
				block.Until = &ms
			case flag == "--reason" && args[0] == "add" && block.Reason == "":
				block.Reason = v
			default:
				return errors.New(blocksUsage)
			}
		}
		if !confirmed {
			return errors.New("refused: repeat the value after --confirm to " + args[0] + " the block")
		}
		if value, err := ingress.NormalizeBlock(block.Kind, block.Value); err == nil {
			id = ingress.BlockID(block.Kind, value)
		}
	default:
		return errors.New(blocksUsage)
	}
	// Another user would create files the services cannot open, or lack access.
	info, err := os.Stat(config.StateDir())
	if err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("run as the owner of %s: docker compose exec --user kypost kypost-server kypost-server receiving blocks <command>", config.StateDir())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := openReceivingRuntime(ctx, false)
	if err != nil {
		return err
	}
	defer r.holding.Close()
	store := ingress.NewBlocks(filepath.Join(r.stateDir, "receiving"))
	switch action {
	case "list":
		list, err := store.List(time.Now())
		if err != nil {
			return err
		}
		evidence := ingress.NewEvidence(filepath.Join(r.stateDir, "receiving")).Status(time.Now())
		return json.NewEncoder(output).Encode(map[string]any{"blocks": list, "evidence": evidence})
	case "add":
		set, err := r.domains.ReadSet()
		if err != nil {
			return err
		}
		added, err := store.Put(ctx, block, slices.Collect(maps.Keys(set.Domains)), time.Now())
		if err != nil {
			return err
		}
		status = "blocked"
		return json.NewEncoder(output).Encode(added)
	default:
		if id == "" {
			return ingress.ErrBlockInvalid
		}
		found, err := store.Remove(ctx, id, time.Now())
		warning := errors.Is(err, ingress.ErrUnblockNotRecorded)
		if err == nil && !found {
			err = errors.New("no such block in force")
		}
		if err != nil && !warning {
			return err
		}
		status = "unblocked"
		if warning {
			slog.Warn("receiving sender evidence", "actor", actor, "task_id", "native-receiving", "action", "unblock_sender", "target", "sender-evidence", "result", "suppression-failed", "correlation_id", id, "error", err.Error())
			_, err = fmt.Fprintln(output, "removed; warning:", ingress.ErrUnblockNotRecorded)
			return err
		}
		_, err = fmt.Fprintln(output, "removed")
		return err
	}
}

// senderEvidence feeds automatic sender blocks from one scanned message: a
// reject verdict counts (Evidence.Reject, each block made audited by ID and
// level, never the address); an accepted one records the authenticated
// domain as good. It runs after the verdict or commit, with its own short
// deadline: on contention it drops the work rather than hold up the SMTP
// reply, and a failure never changes that reply.
func (r *receivingRuntime) senderEvidence(ctx context.Context, deliveryID string, auth ingress.Authentication, reject bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), evidenceTimeout)
	defer cancel()
	evidence, action := ingress.NewEvidence(filepath.Join(r.stateDir, "receiving")), "accepted-domain"
	var err error
	if reject {
		action = "reject"
		var set sso.NativeDomainSet
		var made []ingress.SenderBlock
		if set, err = r.domains.ReadSet(); err == nil {
			made, err = evidence.Reject(ctx, auth, slices.Collect(maps.Keys(set.Domains)), time.Now())
		}
		for _, b := range made {
			slog.Info("receiving sender block change", "actor", "automatic", "task_id", "native-receiving", "action", "block_sender", "target", b.Kind, "result", "blocked", "correlation_id", b.ID, "block_level", b.Level, "until_ms", *b.Until)
		}
	} else {
		err = evidence.Accepted(ctx, auth, time.Now())
	}
	if err != nil {
		result := "dropped"
		if errors.Is(err, ingress.ErrAutomaticFull) {
			result = "automatic-full" // also in the block list's evidence status
		}
		slog.Warn("receiving sender evidence", "actor", "automatic", "task_id", "native-receiving", "action", action, "target", "sender-evidence", "result", result, "correlation_id", deliveryID, "error", err.Error())
	}
}

// evidenceTimeout bounds evidence work on the SMTP path.
var evidenceTimeout = 2 * time.Second
