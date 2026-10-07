//go:build linux

package sso

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeOutboundExpiryDuringDeviceContention(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	owner := mailbox.Owner{Issuer: nativeIssuer, Subject: "one", Mailbox: "local-one"}
	source, err := mailbox.PrepareAccount(root, owner, "one@example.test", nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := state.OpenNative(filepath.Join(root, "users", owner.Mailbox), source)
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	dev := state.NativeDevice{DeviceID: "dev", Platform: "android", PushToken: "push", SecretHash: "credential"}
	if err = devices.UpsertNativeDevice(dev); err != nil {
		t.Fatal(err)
	}
	entered, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		holderDone <- devices.WithNativeSendDevice(ctx, "dev", func(state.NativeDevice) error { close(entered); <-release; return nil })
	}()
	<-entered
	job := mailbox.OutboundJob{From: "one@example.test", RelayGeneration: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", DirectoryRevision: 1, DeviceID: "dev", DeviceWitness: state.NativeDeviceWitness(dev), ExpiresAt: time.Now().Unix() + 1}
	u := users.User{ID: owner.Mailbox}
	a := NativeAssignment{Address: job.From, Source: source}
	ds := DirectoryState{Revision: 1}
	relay := mailmsg.DomainRelay{Generation: job.RelayGeneration}
	done := make(chan error, 1)
	called := false
	go func() {
		done <- (NativeOutbound{StateRoot: root}).withJobAuthority(ctx, u, a, NativeAddress{Address: job.From, State: "active"}, ds, relay, job, nil, func(context.Context) error { called = true; return nil })
	}()
	time.Sleep(time.Until(time.Unix(job.ExpiresAt, 0)) + 100*time.Millisecond)
	close(release)
	if err = <-holderDone; err != nil {
		t.Fatal(err)
	}
	err = <-done
	if err == nil || called {
		t.Fatalf("expired job reached claim callback: called=%t err=%v", called, err)
	}
}

func outboundFixture(t *testing.T) (NativeOutbound, users.User, mailbox.OutboundJob) {
	t.Helper()
	ctx := context.Background()
	config, root, secrets := t.TempDir(), t.TempDir(), t.TempDir()
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	nativeDesired(t, life, "one", "one@example.test", 1, true)
	u, err := life.AllocateNativeAccount(ctx, root, nativeIssuer, "one", domains, accounts, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	settings := NewStore(config)
	if err = settings.Save(SSOSettings{Enabled: true, IssuerURL: nativeIssuer}); err != nil {
		t.Fatal(err)
	}
	_, err = mailmsg.SaveDomainRelay(ctx, filepath.Join(config, "native-relay.json"), filepath.Join(secrets, "native-relay.key"), mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: nativeIssuer, Host: "127.0.0.1", Port: 465, Username: "operator", Password: "test-only"})
	if err != nil {
		t.Fatal(err)
	}
	job := mailbox.OutboundJob{From: "one@example.test", NativeSendEpoch: u.NativeSendEpoch, PGPRevision: u.PGPRevision, Deliveries: []mailbox.OutboundDelivery{{Recipients: []string{"recipient@example.test"}, Raw: []byte("From: one@example.test\r\nTo: recipient@example.test\r\nSubject: retained\r\n\r\nmessage\r\n")}}}
	return NativeOutbound{ConfigDir: config, StateRoot: root, SecretDir: secrets, Accounts: accounts, Domains: domains, Settings: settings}, u, job
}

func TestNativeOutboundRevocationBeforeClaim(t *testing.T) {
	for _, mode := range []string{"deactivate-reactivate", "issuer-disabled", "directory-revision", "relay-generation", "device-repair"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			sender, u, job := outboundFixture(t)
			var devices *state.Store
			if mode == "device-repair" {
				var err error
				devices, err = state.OpenNative(filepath.Join(sender.StateRoot, "users", u.ID), u.NativeMailboxSource)
				if err != nil {
					t.Fatal(err)
				}
				defer devices.Close()
				device := state.NativeDevice{DeviceID: "same-id", SecretHash: "first-credential", Platform: "android", PushToken: "push"}
				if err = devices.UpsertNativeDevice(device); err != nil {
					t.Fatal(err)
				}
				job.DeviceID = device.DeviceID
				job.DeviceWitness = state.NativeDeviceWitness(device)
			}
			id, err := fsutil.NewUUIDv4()
			if err != nil {
				t.Fatal(err)
			}
			if err = sender.Queue(ctx, u.ID, id, job); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "deactivate-reactivate":
				if _, err = sender.Accounts.Deactivate(u.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = sender.Accounts.Reactivate(u.ID); err != nil {
					t.Fatal(err)
				}
				current, err := sender.Accounts.Get(u.ID)
				if err != nil || current.NativeSendEpoch != u.NativeSendEpoch+2 {
					t.Fatal("same-second revocation was lost", current.NativeSendEpoch, err)
				}
			case "issuer-disabled":
				if err = sender.Settings.Save(SSOSettings{IssuerURL: nativeIssuer}); err != nil {
					t.Fatal(err)
				}
			case "directory-revision":
				nativeDesired(t, NewLifecycleStore(sender.ConfigDir), "one", "one@example.test", 2, true)
			case "relay-generation":
				_, err = mailmsg.SaveDomainRelay(ctx, filepath.Join(sender.ConfigDir, "native-relay.json"), sender.keyPath(), mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: nativeIssuer, Host: "127.0.0.1", Port: 465, Username: "operator", Password: "rotated"})
				if err != nil {
					t.Fatal(err)
				}
			case "device-repair":
				device, _ := devices.GetNativeDevice(job.DeviceID)
				device.SecretHash = "replacement-credential"
				if err = devices.UpsertNativeDevice(device); err != nil {
					t.Fatal(err)
				}
			}
			result, err := sender.Submit(ctx, u.ID, id, 0)
			if err == nil || result.Accepted {
				t.Fatal("revoked queued intent submitted", result, err)
			}
			statuses, _, err := sender.Status(ctx, u.ID, id)
			if err != nil || len(statuses) != 1 || statuses[0].State != "queued" || statuses[0].Attempts != 0 {
				t.Fatal("refusal consumed durable claim", statuses, err)
			}
		})
	}
}

func TestNativeOutboundSettingsFence(t *testing.T) {
	config := t.TempDir()
	s := NewStore(config)
	other := NewStore(config)
	if err := s.Save(SSOSettings{Enabled: true, IssuerURL: nativeIssuer}); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.WithCurrentSettings(context.Background(), func(settings SSOSettings) error {
			if !settings.Enabled {
				return errors.New("missing settings")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	saved := make(chan error, 1)
	go func() { saved <- other.Save(SSOSettings{}) }()
	select {
	case err := <-saved:
		close(release)
		t.Fatal("settings write crossed admission", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if s.Load().Enabled {
		t.Fatal("writer never committed")
	}
}

func TestNativeOutboundRecoveryQuarantinesRevokedJob(t *testing.T) {
	ctx := context.Background()
	sender, u, job := outboundFixture(t)
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Queue(ctx, u.ID, id, job); err != nil {
		t.Fatal(err)
	}
	if _, err = sender.Accounts.Deactivate(u.ID); err != nil {
		t.Fatal(err)
	}
	if err = sender.Recover(ctx, u.ID, id); err == nil {
		t.Fatal("inactive sender accepted")
	}
	statuses, _, err := sender.Status(ctx, u.ID, id)
	if err != nil || len(statuses) != 1 || statuses[0].State != "quarantined" || statuses[0].Attempts != 0 {
		t.Fatal("revoked work not retained without network", statuses, err)
	}
	pending, err := sender.Pending(ctx, u.ID, 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("quarantined job rescheduled", pending, err)
	}
}

func TestNativeOutboundSettingsReadDoesNotInvertDirectoryFence(t *testing.T) {
	config := t.TempDir()
	s := NewStore(config)
	life := NewLifecycleStore(config)
	if err := s.Save(SSOSettings{Enabled: true, IssuerURL: nativeIssuer}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	directoryRelease, err := life.LockDirectoryContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryRelease()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.WithCurrentSettings(ctx, func(SSOSettings) error { close(entered); <-release; return nil })
	}()
	<-entered
	saved := make(chan error, 1)
	go func() { saved <- s.Save(SSOSettings{}) }()
	read := make(chan SSOSettings, 1)
	go func() { read <- s.Load() }()
	select {
	case settings := <-read:
		if !settings.Enabled {
			t.Fatal("in-flight settings lost")
		}
	case <-ctx.Done():
		close(release)
		t.Fatal("settings read blocked behind writer while holding directory")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
}

func TestNativeOutboundConvertedDeviceKeyFence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	owner := mailbox.Owner{Issuer: nativeIssuer, Subject: "one", Mailbox: "local-one"}
	source, err := mailbox.PrepareAccount(root, owner, "one@example.test", nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := state.OpenNative(filepath.Join(root, "users", owner.Mailbox), source)
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	device := state.NativeDevice{DeviceID: "device", Platform: "android", PushToken: "push", SecretHash: "credential", EncryptionEnrolled: true, EnrolledGeneration: 2, EnrolledFingerprint: "current-fingerprint"}
	u := users.User{ID: owner.Mailbox, NativeMailboxSource: source, PGPRevision: 7, PGPFingerprint: device.EnrolledFingerprint, PGPKeyring: &users.PGPKeyringState{MaterialGeneration: 2}}
	job := mailbox.OutboundJob{From: "one@example.test", RelayGeneration: "generation", DirectoryRevision: 1, PGPRevision: u.PGPRevision, PGPFingerprint: u.PGPFingerprint, MaterialGeneration: 2, DeviceID: device.DeviceID, DeviceWitness: state.NativeDeviceWitness(device), RequiresEnrollment: true}
	a := NativeAssignment{Address: job.From, Source: source}
	d := DirectoryState{Revision: 1}
	relay := mailmsg.DomainRelay{Generation: job.RelayGeneration}
	for _, mode := range []string{"current", "unenrolled", "retired-generation", "different-fingerprint", "old-revision"} {
		t.Run(mode, func(t *testing.T) {
			current, prepared := device, job
			switch mode {
			case "unenrolled":
				current.EncryptionEnrolled = false
			case "retired-generation":
				current.EnrolledGeneration = 1
			case "different-fingerprint":
				current.EnrolledFingerprint = "retired"
			case "old-revision":
				prepared.PGPRevision--
			}
			if err := devices.UpsertNativeDevice(current); err != nil {
				t.Fatal(err)
			}
			enrollment := state.DeviceEnrollment{Version: 3, Generation: current.EnrolledGeneration, Fingerprint: current.EnrolledFingerprint}
			if err := devices.RecordNativeDeviceDelivery(current.DeviceID, enrollment); err != nil {
				t.Fatal(err)
			}
			if current.EncryptionEnrolled {
				if ok, err := devices.ConfirmNativeDeviceEnrollment(current.DeviceID, enrollment); err != nil || !ok {
					t.Fatal("enrollment fixture not confirmed", err)
				}
			}
			called := false
			err := (NativeOutbound{StateRoot: root}).withJobAuthority(ctx, u, a, NativeAddress{Address: job.From, State: "active"}, d, relay, prepared, nil, func(context.Context) error { called = true; return nil })
			if mode == "current" {
				if err != nil || !called {
					t.Fatal("current enrolled key refused", err)
				}
			} else if err == nil || called {
				t.Fatal("retired device/key authority admitted", mode, err)
			}
		})
	}
}
