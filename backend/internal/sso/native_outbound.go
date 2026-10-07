package sso

import (
	"context"
	"errors"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
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
	Logger                          *logging.Logger
	Accounts                        *users.Store
	Domains                         *NativeDomainStore
	Settings                        *Store
}

type NativeOutboundResult struct{ Accepted, SentSaved bool }
type nativeOutboundClaim struct {
	id, token, from, source, dir string
	sequence                     int
	owner                        mailbox.Owner
	limits                       mailbox.Limits
	relay                        mailmsg.DomainRelay
	delivery                     mailbox.OutboundDelivery
}

func (s NativeOutbound) keyPath() string { return filepath.Join(s.SecretDir, "native-relay.key") }

// DNS precedes every disk fence. Lock order is domain -> settings -> directory
// -> users -> device SQLite -> mailbox SQLite. Network never runs in action.
// A lapsed proof does not skip the staleness checks: action still runs with
// unproven set, so a stale job is quarantined even while DNS is down, and
// withJobAuthority refuses to commit (the job stays for retry). x is the
// current ledger record of from (zero when unknown), read under the fence.
// mailboxID selects the sending mailbox; a primary one's ID is its user's.
func (s NativeOutbound) withAuthority(ctx context.Context, mailboxID, from string, action func(ctx context.Context, u users.User, a NativeAssignment, x NativeAddress, d DirectoryState, relay mailmsg.DomainRelay, box *mailbox.Store, key []byte, unproven error) error) error {
	if s.Accounts == nil || s.Domains == nil || s.Settings == nil || s.ConfigDir == "" || s.StateRoot == "" || s.SecretDir == "" || !fsutil.SafePathComponent(mailboxID) || !strings.Contains(from, "@") || action == nil {
		return ErrNativeProvisioning
	}
	if err := RequireNativeRestoreReleased(s.StateRoot); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	life := NewLifecycleStore(s.ConfigDir)
	sender, known, err := life.NativeMailboxAssignment(mailboxID)
	if err != nil {
		return err
	}
	if !known || sender.Address == "" {
		return ErrNativeProvisioning
	}
	userID := sender.UserID()
	domain := AddressDomain(from)
	proof, unproven := s.Domains.VerifyDomain(ctx, domain)
	if unproven == nil {
		var expire context.CancelFunc
		ctx, expire = context.WithDeadline(ctx, time.Unix(proof.VerifiedUntil, 0))
		defer expire()
	}
	release, err := fsutil.LockFileContext(ctx, filepath.Join(s.ConfigDir, NativeDomainsFile))
	if err != nil {
		return err
	}
	defer release()
	current, err := s.Domains.ReadSet()
	if err != nil {
		return err
	}
	// Unconfigured or retired never comes back: end the job. A lapsed or
	// changed proof on a configured domain is retried.
	if _, configured := current.Domains[domain]; !configured {
		return ErrNativeOutboundStale
	}
	if unproven == nil && !current.CurrentProof(proof) {
		return ErrNativeDomain
	}
	return s.Settings.WithCurrentSettings(ctx, func(settings SSOSettings) error {
		if !settings.Enabled || settings.IssuerURL != current.Issuer {
			return ErrNativeDomain
		}
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
				a, err := life.admitNativeMailbox(ctx, s.StateRoot, current.Issuer, u, mailboxID)
				if err != nil {
					return err
				}
				addresses, err := life.NativeAddresses()
				if err != nil {
					return err
				}
				if u.MustChangePassword {
					return ErrNativeOutboundStale
				}
				d, known, err := life.Directory(current.Issuer, u.SSOSub)
				if err != nil || !known {
					return ErrNativeProvisioning
				}
				relay, exists, err := mailmsg.ReadDomainRelay(filepath.Join(s.ConfigDir, "native-relay.json"), s.keyPath())
				if err != nil || !exists || relay.Issuer != current.Issuer {
					return mailmsg.ErrDomainRelay
				}
				if !relay.Sends(domain) {
					return ErrNativeOutboundStale
				}
				key, err := cryptutil.LoadKey(s.keyPath())
				if err != nil {
					return mailmsg.ErrDomainRelay
				}
				box, err := mailbox.OpenExisting(filepath.Join(a.Dir(s.StateRoot), "mailbox"), a.Owner, a.Limits, a.Source)
				if err != nil {
					return err
				}
				defer box.Close()
				if err := RequireNativeRestoreReleased(s.StateRoot); err != nil {
					return err
				}
				return action(ctx, u, a, addresses[from], d, relay, box, key, unproven)
			}
			return ErrNativeProvisioning
		})
	})
}

// unproven, when set, is returned instead of committing once every staleness
// check has passed.
func (s NativeOutbound) withJobAuthority(ctx context.Context, u users.User, a NativeAssignment, from NativeAddress, d DirectoryState, relay mailmsg.DomainRelay, job mailbox.OutboundJob, unproven error, action func(context.Context) error) error {
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
		if unproven != nil {
			return unproven
		}
		return action(ctx)
	}
	// From must be an active ledger address of this mailbox at the recorded
	// generation. Jobs queued before generations existed carry 0 and the
	// primary; the directory-revision fence below still covers them. A
	// verified legacy send-as row never transfers native domain authority.
	if job.ExpiresAt != 0 && job.ExpiresAt <= time.Now().Unix() {
		return ErrNativeOutboundStale
	}
	if job.From != from.Address || from.Mailbox != a.Owner.Mailbox || from.State != "active" || job.FromGeneration != from.Generation && (job.FromGeneration != 0 || job.From != a.Address) {
		return ErrNativeOutboundStale
	}
	if job.RelayGeneration != relay.Generation || job.NativeSendEpoch != u.NativeSendEpoch || job.DirectoryRevision != d.Revision || job.PGPRevision != u.PGPRevision || job.PGPFingerprint != u.PGPFingerprint {
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
	// Devices live only in the owner's primary state, under its source.
	devices, err := state.OpenNative(filepath.Join(s.StateRoot, "users", u.ID), u.NativeMailboxSource)
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
func (s NativeOutbound) queue(ctx context.Context, mailboxID, id string, job mailbox.OutboundJob, claimPrimary bool) (nativeOutboundClaim, error) {
	var claim nativeOutboundClaim
	err := s.withAuthority(ctx, mailboxID, job.From, func(ctx context.Context, u users.User, a NativeAssignment, x NativeAddress, d DirectoryState, relay mailmsg.DomainRelay, box *mailbox.Store, key []byte, unproven error) error {
		if job.NativeSendEpoch != u.NativeSendEpoch || job.PGPRevision != u.PGPRevision {
			return ErrNativeOutboundStale
		}
		if job.RelayGeneration != "" && job.RelayGeneration != relay.Generation {
			return ErrNativeOutboundStale
		}
		job.RelayGeneration = relay.Generation
		job.DirectoryRevision = d.Revision
		job.FromGeneration = x.Generation
		job.PGPFingerprint = u.PGPFingerprint
		return s.withJobAuthority(ctx, u, a, x, d, relay, job, unproven, func(ctx context.Context) error {
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
			claim = nativeOutboundClaim{id: id, token: token, from: job.From, source: a.Source, dir: a.Dir(s.StateRoot), owner: a.Owner, limits: a.Limits, relay: relay, delivery: delivery}
			return nil
		})
	})
	return claim, err
}

// Queue persists intent without contacting the provider. It is never a send
// success response. Internal callers supply a server-generated immutable ID.
// Every mailboxID below names the sending mailbox; a primary's is its user ID.
func (s NativeOutbound) Queue(ctx context.Context, mailboxID, id string, job mailbox.OutboundJob) error {
	_, err := s.queue(ctx, mailboxID, id, job, false)
	return err
}

// Send records every delivery and Sent obligation before dialing the first.
func (s NativeOutbound) Send(ctx context.Context, mailboxID, id string, job mailbox.OutboundJob) (NativeOutboundResult, error) {
	claim, err := s.queue(ctx, mailboxID, id, job, true)
	if err != nil {
		return NativeOutboundResult{}, err
	}
	return s.deliver(ctx, claim)
}

// Submit rechecks current authority for a stored attempt, never a cached grant.
func (s NativeOutbound) Submit(ctx context.Context, mailboxID, id string, sequence int) (NativeOutboundResult, error) {
	// The stored From selects the domain to prove; the job is reread under the fence.
	box, key, err := s.openStorage(mailboxID)
	if err != nil {
		return NativeOutboundResult{}, err
	}
	stored, _, _, err := box.ReadOutbound(ctx, key, id)
	_ = box.Close()
	if err != nil {
		return NativeOutboundResult{}, err
	}
	var claim nativeOutboundClaim
	err = s.withAuthority(ctx, mailboxID, stored.From, func(ctx context.Context, u users.User, a NativeAssignment, x NativeAddress, d DirectoryState, relay mailmsg.DomainRelay, box *mailbox.Store, key []byte, unproven error) error {
		job, statuses, _, err := box.ReadOutbound(ctx, key, id)
		if err != nil {
			return err
		}
		if sequence > 0 && (len(statuses) == 0 || statuses[0].State != "accepted") {
			return mailbox.ErrOutbound
		}
		return s.withJobAuthority(ctx, u, a, x, d, relay, job, unproven, func(ctx context.Context) error {
			delivery, token, err := box.ClaimOutbound(ctx, key, id, sequence, relay.Generation)
			if err != nil {
				return err
			}
			claim = nativeOutboundClaim{id: id, token: token, from: job.From, sequence: sequence, source: a.Source, dir: a.Dir(s.StateRoot), owner: a.Owner, limits: a.Limits, relay: relay, delivery: delivery}
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
	if submission != nil && s.Logger != nil {
		// Provider text can echo credentials or correspondence. Retain only a
		// bounded reply code and existing acceptance classification, never Error().
		reason := "transport_error"
		var reply *textproto.Error
		if errors.As(submission, &reply) && reply.Code >= 400 && reply.Code <= 599 {
			reason = "smtp_" + strconv.Itoa(reply.Code)
		}
		outcome := "not_confirmed"
		if errors.Is(submission, mailmsg.ErrSMTPAcceptanceUncertain) {
			outcome = "uncertain"
		} else if result.Accepted {
			outcome = "accepted_teardown_failed"
		}
		s.Logger.Error("native relay attempt requires delivery evidence before resubmitting", "correlation_id", c.id, "slot", strconv.Itoa(c.sequence), "reason", reason, "result", outcome)
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	box, err := mailbox.OpenExisting(filepath.Join(c.dir, "mailbox"), c.owner, c.limits, c.source)
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
// inactive users' and disabled mailboxes' accepted Sent obligations to finish,
// without network authority.
func (s NativeOutbound) openStorage(mailboxID string) (*mailbox.Store, []byte, error) {
	if !fsutil.SafePathComponent(mailboxID) || s.Accounts == nil {
		return nil, nil, ErrNativeProvisioning
	}
	life := NewLifecycleStore(s.ConfigDir)
	a, known, err := life.NativeMailboxAssignment(mailboxID)
	if err != nil || !known || a.Source == "" {
		return nil, nil, ErrNativeProvisioning
	}
	u, err := s.Accounts.Get(a.UserID())
	if err != nil || u.NativeMailboxIssuer != a.Owner.Issuer || u.SSOSub != a.Owner.Subject || u.NativeMailboxSource == "" {
		return nil, nil, ErrNativeProvisioning
	}
	if err := life.ValidateNativeUserStorage(s.StateRoot, u); err != nil {
		return nil, nil, err
	}
	if source, err := mailbox.ValidatePreparedMailbox(filepath.Dir(a.Dir(s.StateRoot)), a.Owner, a.Address, a.Limits); err != nil || source != a.Source {
		return nil, nil, ErrNativeProvisioning
	}
	key, err := cryptutil.LoadKey(s.keyPath())
	if err != nil {
		return nil, nil, mailbox.ErrOutbound
	}
	box, err := mailbox.OpenExisting(filepath.Join(a.Dir(s.StateRoot), "mailbox"), a.Owner, a.Limits, a.Source)
	return box, key, err
}

func (s NativeOutbound) Status(ctx context.Context, mailboxID, id string) ([]mailbox.OutboundStatus, bool, error) {
	box, key, err := s.openStorage(mailboxID)
	if err != nil {
		return nil, false, err
	}
	defer box.Close()
	job, statuses, sent, err := box.ReadOutbound(ctx, key, id)
	return statuses, len(job.Sent) > 0 && sent > 0, err
}
func (s NativeOutbound) Pending(ctx context.Context, mailboxID string, limit int) ([]string, error) {
	box, _, err := s.openStorage(mailboxID)
	if err != nil {
		return nil, err
	}
	defer box.Close()
	return box.PendingOutbound(ctx, limit)
}
func (s NativeOutbound) FileSent(ctx context.Context, mailboxID, id string) error {
	box, key, err := s.openStorage(mailboxID)
	if err != nil {
		return err
	}
	defer box.Close()
	_, err = box.FileOutboundSent(ctx, key, id)
	return err
}
func (s NativeOutbound) Quarantine(ctx context.Context, mailboxID, id string, sequence int) error {
	box, _, err := s.openStorage(mailboxID)
	if err != nil {
		return err
	}
	defer box.Close()
	return box.QuarantineOutbound(ctx, id, sequence)
}

// Recover attempts only due, definitely unsent work. A primary must be accepted
// before follow-ons; ambiguous/crashed attempts never authorize an automatic retry.
func (s NativeOutbound) Recover(ctx context.Context, mailboxID, id string) error {
	box, key, err := s.openStorage(mailboxID)
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
		filingErr = s.FileSent(ctx, mailboxID, id)
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
		result, submitErr := s.Submit(ctx, mailboxID, id, status.Sequence)
		if status.Sequence == 0 {
			primaryAccepted = result.Accepted
		}
		if submitErr != nil {
			if errors.Is(submitErr, ErrNativeOutboundStale) || errors.Is(submitErr, ErrNativeProvisioning) || errors.Is(submitErr, state.ErrNativeSendDevice) {
				if quarantineErr := s.Quarantine(ctx, mailboxID, id, status.Sequence); quarantineErr != nil {
					return quarantineErr
				}
			}
			return errors.Join(filingErr, submitErr)
		}
	}
	return filingErr
}
