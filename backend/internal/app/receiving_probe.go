package app

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// The hosted profile's two bind checks (RX "Exit codes", "The masked relay domain"). Both
// run only in multi mode (r.tenancy set); single-tenant binds never reach them.

// The refused-recipient budget (RX open question 5): relay-domain refusals per source IP
// (IPv4 /32, IPv6 /64), probeLimit in probeWindow, then exit 13 for probeBlock.
var (
	probeLimit  = 50
	probeWindow = 10 * time.Minute
	probeBlock  = 10 * time.Minute
)

var (
	errOrgUnavailable = &receivingCommandError{err: errors.New("the recipient's org is not receiving now"), code: 12}
	errProbeBudget    = &receivingCommandError{err: errors.New("too many refused relay-domain recipients from this source"), code: 13}
	errProbeSource    = errors.New("a relay-domain recipient needs the peer IP to apply its probe budget")
)

// orgReceives is K's org check after the assignment: a suspended, disabled or uncached
// org is exit 12 (451 4.2.1), deletion_scheduled and deleted the unknown-address 550.
// The staff org always receives (K8).
func (r *receivingRuntime) orgReceives(ctx context.Context, a sso.NativeAssignment) error {
	if r.tenancy == nil {
		return nil
	}
	org, err := tenancy.SubjectOrg(ctx, r.tenancy, a.Owner.Issuer, a.Owner.Subject)
	if err != nil {
		return err
	}
	if _, staff, err := config.Tenancy(); err != nil || org != "" && org == staff {
		return err
	}
	if org == "" {
		return errOrgUnavailable
	}
	o, err := tenancy.LookupOrg(ctx, r.tenancy, org)
	if err != nil {
		return err
	}
	switch o.Receiving() {
	case tenancy.ReceiveAccept:
		return nil
	case tenancy.ReceiveUnknown:
		return ingress.ErrRoute
	}
	return errOrgUnavailable
}

// probeBudget applies the budget to a relay-domain recipient: past it, exit 13 for live
// and unknown addresses alike. Otherwise it returns the call that counts a refusal, or
// nil for a primary domain, which is never budgeted. The relay domains are the install's
// masked-address domains (M D2: one platform relay domain, apart from the primaries).
func (r *receivingRuntime) probeBudget(ctx context.Context, recipient, source string) (func(context.Context) error, error) {
	if r.tenancy == nil {
		return nil, nil
	}
	if r.state == nil {
		return nil, errProbeSource
	}
	relay, err := r.state.Setting(state.MaskedDomainsSetting)
	if errors.Is(err, state.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !slices.Contains(strings.Split(relay, ","), sso.AddressDomain(strings.ToLower(recipient))) {
		return nil, nil
	}
	ip, err := netip.ParseAddr(source)
	if err != nil || ip.Zone() != "" {
		return nil, errProbeSource
	}
	ip = ip.Unmap()
	bits := 32
	if ip.Is6() {
		bits = 64
	}
	prefix := netip.PrefixFrom(ip, bits).Masked().String()
	blocked := "receiving-probe-block:" + prefix
	if hits, _, err := r.state.AttemptHits(ctx, blocked); err != nil || hits > 0 {
		if err == nil {
			err = errProbeBudget
		}
		return nil, err
	}
	return func(ctx context.Context) error {
		// The probeLimit-th refusal in a window starts the block.
		_, _, ok, err := r.state.TakeAttempt(ctx, "receiving-probe:"+prefix, probeLimit-1, probeWindow)
		if err == nil && !ok {
			_, _, _, err = r.state.TakeAttempt(ctx, blocked, 1, probeBlock)
		}
		return err
	}, nil
}
