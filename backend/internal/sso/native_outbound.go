package sso

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

var ErrNativeOutboundStale = errors.New("queued sender authority changed; retain the job and reconcile instead of resubmitting")

// NativeOutbound coordinates current authority and durable storage, shared by
// foreground sends and recovery workers. Construction alone authorizes nothing.
// Callers select this only in explicit native mode and preflight MIME/PGP policy.
type NativeOutbound struct {
	ConfigDir, StateRoot, SecretDir string
	Accounts                        *users.Store
	Domains                         *NativeDomainStore
	Settings                        *Store
}

type NativeOutboundResult struct{ Accepted, SentSaved bool }
type nativeOutboundClaim struct {
	id, token, from, source string
	sequence                int
	owner                   mailbox.Owner
	limits                  mailbox.Limits
	relay                   mailmsg.DomainRelay
	delivery                mailbox.OutboundDelivery
}

func (s NativeOutbound) keyPath() string { return filepath.Join(s.SecretDir, "native-relay.key") }

// DNS precedes every disk fence. Lock order is domain -> settings -> directory
// -> users -> device SQLite -> mailbox SQLite. Network never runs in action.
func (s NativeOutbound) withAuthority(ctx context.Context, userID string, action func(context.Context, users.User, NativeAssignment, DirectoryState, mailmsg.DomainRelay, *mailbox.Store, []byte) error) error {
	if s.Accounts == nil || s.Domains == nil || s.Settings == nil || s.ConfigDir == "" || s.StateRoot == "" || s.SecretDir == "" || !fsutil.SafePathComponent(userID) || action == nil {
		return ErrNativeProvisioning
	}
	if err := RequireNativeRestoreReleased(s.StateRoot); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	proof, err := s.Domains.Verify(ctx)
	if err != nil {
		return err
	}
	ctx, expire := context.WithDeadline(ctx, time.Unix(proof.VerifiedUntil, 0))
	defer expire()
	release, err := fsutil.LockFileContext(ctx, filepath.Join(s.ConfigDir, "native-domain.json"))
	if err != nil {
		return err
	}
	defer release()
	current, err := s.Domains.Read()
	if err != nil || current != proof || proof.VerifiedUntil <= time.Now().Unix() {
		return ErrNativeDomain
	}
	return s.Settings.WithCurrentSettings(ctx, func(settings SSOSettings) error {
		if !settings.Enabled || settings.IssuerURL != proof.Issuer {
			return ErrNativeDomain
		}
		life := NewLifecycleStore(s.ConfigDir)
		release, err := life.LockDirectoryContext(ctx)
		if err != nil {
			return err
		}
		defer release()
		return s.Accounts.WithCurrentUsers(ctx, func(all []users.User) error {
			for _, u := range all {
				if u.ID != userID {
					continue
				}
				a, err := life.admitNativeMailUser(ctx, s.StateRoot, proof.Issuer, u)
				if err != nil {
					return err
				}
				if u.MustChangePassword {
					return ErrNativeOutboundStale
				}
				d, known, err := life.Directory(proof.Issuer, u.SSOSub)
				if err != nil || !known {
					return ErrNativeProvisioning
				}
				relay, exists, err := mailmsg.ReadDomainRelay(filepath.Join(s.ConfigDir, "native-relay.json"), s.keyPath())
				if err != nil || !exists || relay.Issuer != proof.Issuer || relay.Domain != proof.Domain {
					return mailmsg.ErrDomainRelay
				}
				key, err := cryptutil.LoadKey(s.keyPath())
				if err != nil {
					return mailmsg.ErrDomainRelay
				}
				box, err := mailbox.OpenExisting(filepath.Join(s.StateRoot, "users", userID, "mailbox"), a.Owner, a.Limits, a.Source)
				if err != nil {
					return err
				}
				defer box.Close()
				if err := RequireNativeRestoreReleased(s.StateRoot); err != nil {
					return err
				}
				return action(ctx, u, a, d, relay, box, key)
			}
			return ErrNativeProvisioning
		})
	})
}

func (s NativeOutbound) withJobAuthority(ctx context.Context, u users.User, a NativeAssignment, d DirectoryState, relay mailmsg.DomainRelay, job mailbox.OutboundJob, action func(context.Context) error) error {
	if job.ExpiresAt != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Unix(job.ExpiresAt, 0))
		defer cancel()
	}
	commit := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if job.ExpiresAt != 0 && job.ExpiresAt <= time.Now().Unix() {
			return ErrNativeOutboundStale
		}
		return action(ctx)
	}
	// Primary addresses only until explicit native alias ownership/routing exists.
	// A verified legacy send-as row must never transfer native domain authority.
	if job.ExpiresAt != 0 && job.ExpiresAt <= time.Now().Unix() {
		return ErrNativeOutboundStale
	}
	if job.From != a.Address || job.RelayGeneration != relay.Generation || job.NativeSendEpoch != u.NativeSendEpoch || job.DirectoryRevision != d.Revision || job.PGPRevision != u.PGPRevision || job.PGPFingerprint != u.PGPFingerprint {
		return ErrNativeOutboundStale
	}
	if u.PGPKeyring != nil {
		if job.MaterialGeneration != u.PGPKeyring.MaterialGeneration {
			return ErrNativeOutboundStale
		}
	} else if job.MaterialGeneration != 0 {
		return ErrNativeOutboundStale
	}
	if job.DeviceID == "" {
		if job.DeviceWitness != "" {
			return ErrNativeOutboundStale
		}
		return commit()
	}
	devices, err := state.OpenNative(filepath.Join(s.StateRoot, "users", u.ID), a.Source)
	if err != nil {
		return err
	}
	defer devices.Close()
	return devices.WithNativeSendDevice(ctx, job.DeviceID, func(device state.NativeDevice) error {
		if job.DeviceWitness == "" || job.DeviceWitness != state.NativeDeviceWitness(device) || device.UserID != "" && device.UserID != u.ID {
			return ErrNativeOutboundStale
		}
		if job.RequiresEnrollment && u.PGPKeyring != nil && (!device.EncryptionEnrolled || device.EnrolledGeneration != job.MaterialGeneration || !strings.EqualFold(device.EnrolledFingerprint, u.PGPFingerprint)) {
			return ErrNativeOutboundStale
		}
		return commit()
	})
}

// queue holds current authority through both enqueue and the primary claim so
// a worker cannot win the first attempt between the foreground's transactions.
func (s NativeOutbound) queue(ctx context.Context, userID, id string, job mailbox.OutboundJob, claimPrimary bool) (nativeOutboundClaim, error) {
	var claim nativeOutboundClaim
	err := s.withAuthority(ctx, userID, func(ctx context.Context, u users.User, a NativeAssignment, d DirectoryState, relay mailmsg.DomainRelay, box *mailbox.Store, key []byte) error {
		if job.NativeSendEpoch != u.NativeSendEpoch || job.PGPRevision != u.PGPRevision {
			return ErrNativeOutboundStale
		}
		if job.RelayGeneration != "" && job.RelayGeneration != relay.Generation {
			return ErrNativeOutboundStale
		}
		job.RelayGeneration = relay.Generation
		job.DirectoryRevision = d.Revision
		job.PGPFingerprint = u.PGPFingerprint
		return s.withJobAuthority(ctx, u, a, d, relay, job, func(ctx context.Context) error {
			if err := box.QueueOutbound(ctx, key, id, job); err != nil {
				return err
			}
			if !claimPrimary {
				return nil
			}
			delivery, token, err := box.ClaimOutbound(ctx, key, id, 0, relay.Generation)
			if err != nil {
				return err
			}
			claim = nativeOutboundClaim{id: id, token: token, from: job.From, source: a.Source, owner: a.Owner, limits: a.Limits, relay: relay, delivery: delivery}
			return nil
		})
	})
	return claim, err
}

// Queue persists intent without contacting the provider. It is never a send
// success response. Internal callers supply a server-generated immutable ID.
func (s NativeOutbound) Queue(ctx context.Context, userID, id string, job mailbox.OutboundJob) error {
	_, err := s.queue(ctx, userID, id, job, false)
	return err
}

// Send records every delivery and Sent obligation before dialing the first.
func (s NativeOutbound) Send(ctx context.Context, userID, id string, job mailbox.OutboundJob) (NativeOutboundResult, error) {
	claim, err := s.queue(ctx, userID, id, job, true)
	if err != nil {
		return NativeOutboundResult{}, err
	}
	return s.deliver(ctx, claim)
}

// Submit rechecks current authority for a stored attempt, never a cached grant.
func (s NativeOutbound) Submit(ctx context.Context, userID, id string, sequence int) (NativeOutboundResult, error) {
	var claim nativeOutboundClaim
	err := s.withAuthority(ctx, userID, func(ctx context.Context, u users.User, a NativeAssignment, d DirectoryState, relay mailmsg.DomainRelay, box *mailbox.Store, key []byte) error {
		job, statuses, _, err := box.ReadOutbound(ctx, key, id)
		if err != nil {
			return err
		}
		if sequence > 0 && (len(statuses) == 0 || statuses[0].State != "accepted") {
			return mailbox.ErrOutbound
		}
		return s.withJobAuthority(ctx, u, a, d, relay, job, func(ctx context.Context) error {
			delivery, token, err := box.ClaimOutbound(ctx, key, id, sequence, relay.Generation)
			if err != nil {
				return err
			}
			claim = nativeOutboundClaim{id: id, token: token, from: job.From, sequence: sequence, source: a.Source, owner: a.Owner, limits: a.Limits, relay: relay, delivery: delivery}
			return nil
		})
	})
	if err != nil {
		return NativeOutboundResult{}, err
	}
	return s.deliver(ctx, claim)
}

func (s NativeOutbound) deliver(ctx context.Context, c nativeOutboundClaim) (NativeOutboundResult, error) {
	// All authority/device locks are released before TLS/AUTH/DATA. Record the
	// real outcome even if the HTTP caller disconnected after the durable claim.
	submission := c.relay.Deliver(c.from, c.delivery.Recipients, c.delivery.Raw)
	result := NativeOutboundResult{Accepted: submission == nil || errors.Is(submission, mailmsg.ErrSMTPAcceptedThenFailed)}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	box, err := mailbox.OpenExisting(filepath.Join(s.StateRoot, "users", c.owner.Mailbox, "mailbox"), c.owner, c.limits, c.source)
	if err != nil {
		return result, mailbox.ErrOutbound
	}
	defer box.Close()
	if err := box.CompleteOutbound(finish, c.id, c.sequence, c.token, submission); err != nil {
		return result, mailbox.ErrOutbound
	}
	if result.Accepted {
		key, err := cryptutil.LoadKey(s.keyPath())
		if err == nil {
			_, err = box.FileOutboundSent(finish, key, c.id)
		}
		result.SentSaved = err == nil
	}
	return result, submission
}

// openStorage proves immutable historical storage only. It deliberately allows
// inactive users' accepted Sent obligations to finish, without network authority.
func (s NativeOutbound) openStorage(userID string) (*mailbox.Store, []byte, error) {
	if !fsutil.SafePathComponent(userID) || s.Accounts == nil {
		return nil, nil, ErrNativeProvisioning
	}
	u, err := s.Accounts.Get(userID)
	if err != nil || u.NativeMailboxIssuer == "" || u.NativeMailboxSource == "" {
		return nil, nil, ErrNativeProvisioning
	}
	life := NewLifecycleStore(s.ConfigDir)
	if err := life.ValidateNativeUserStorage(s.StateRoot, u); err != nil {
		return nil, nil, err
	}
	a, known, err := life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil || !known {
		return nil, nil, ErrNativeProvisioning
	}
	key, err := cryptutil.LoadKey(s.keyPath())
	if err != nil {
		return nil, nil, mailbox.ErrOutbound
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.StateRoot, "users", userID, "mailbox"), a.Owner, a.Limits, a.Source)
	return box, key, err
}

func (s NativeOutbound) Status(ctx context.Context, userID, id string) ([]mailbox.OutboundStatus, bool, error) {
	box, key, err := s.openStorage(userID)
	if err != nil {
		return nil, false, err
	}
	defer box.Close()
	job, statuses, sent, err := box.ReadOutbound(ctx, key, id)
	return statuses, len(job.Sent) > 0 && sent > 0, err
}
func (s NativeOutbound) Pending(ctx context.Context, userID string, limit int) ([]string, error) {
	box, _, err := s.openStorage(userID)
	if err != nil {
		return nil, err
	}
	defer box.Close()
	return box.PendingOutbound(ctx, limit)
}
func (s NativeOutbound) FileSent(ctx context.Context, userID, id string) error {
	box, key, err := s.openStorage(userID)
	if err != nil {
		return err
	}
	defer box.Close()
	_, err = box.FileOutboundSent(ctx, key, id)
	return err
}
func (s NativeOutbound) Quarantine(ctx context.Context, userID, id string, sequence int) error {
	box, _, err := s.openStorage(userID)
	if err != nil {
		return err
	}
	defer box.Close()
	return box.QuarantineOutbound(ctx, id, sequence)
}

// Recover attempts only due, definitely unsent work. A primary must be accepted
// before follow-ons; ambiguous/crashed attempts never authorize an automatic retry.
func (s NativeOutbound) Recover(ctx context.Context, userID, id string) error {
	box, key, err := s.openStorage(userID)
	if err != nil {
		return err
	}
	job, statuses, sent, err := box.ReadOutbound(ctx, key, id)
	_ = box.Close()
	if err != nil {
		return err
	}
	if len(statuses) == 0 {
		return mailbox.ErrOutbound
	}
	primaryAccepted := statuses[0].State == "accepted"
	var filingErr error
	if primaryAccepted && len(job.Sent) > 0 && sent == 0 {
		filingErr = s.FileSent(ctx, userID, id)
	}
	for _, status := range statuses {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if status.Sequence > 0 && !primaryAccepted {
			break
		}
		if (status.State != "queued" && status.State != "retryable") || status.NextAttempt > time.Now().Unix() {
			continue
		}
		result, submitErr := s.Submit(ctx, userID, id, status.Sequence)
		if status.Sequence == 0 {
			primaryAccepted = result.Accepted
		}
		if submitErr != nil {
			if errors.Is(submitErr, ErrNativeOutboundStale) || errors.Is(submitErr, ErrNativeProvisioning) || errors.Is(submitErr, state.ErrNativeSendDevice) {
				if quarantineErr := s.Quarantine(ctx, userID, id, status.Sequence); quarantineErr != nil {
					return quarantineErr
				}
			}
			return errors.Join(filingErr, submitErr)
		}
	}
	return filingErr
}
