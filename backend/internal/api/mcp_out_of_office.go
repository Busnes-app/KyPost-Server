package api

import (
	"context"
	"errors"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// msgRemoteOOO refuses an agent change that would reply to strangers.
const msgRemoteOOO = "agents cannot turn on or change the reply to outside senders; the user does that in KyPost"

type oooAgentView struct {
	Enabled      bool   `json:"enabled"`
	Active       bool   `json:"active"`
	Start        string `json:"start,omitempty"`
	End          string `json:"end,omitempty"`
	TimeZone     string `json:"time_zone,omitempty"`
	LocalMessage string `json:"local_message"`
	Remote       string `json:"remote" jsonschema:"off, on or contacts; agents cannot change it"`
}

func (t mcpTools) oooAgentView(o config.OutOfOffice) oooAgentView {
	v := t.s.oooView(t.caller.user.ID, o)
	return oooAgentView{Enabled: v.Enabled, Active: v.Active, Start: v.Start, End: v.End, TimeZone: v.TimeZone, LocalMessage: v.LocalMessage, Remote: v.Remote}
}

func (t mcpTools) getOutOfOffice(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, oooAgentView, error) {
	o, err := t.s.loadOutOfOffice(t.caller.user.ID)
	if err != nil {
		return nil, oooAgentView{}, errors.New("settings unavailable")
	}
	return nil, t.oooAgentView(o), nil
}

type setOOOIn struct {
	Enabled      bool    `json:"enabled"`
	Start        *string `json:"start,omitempty" jsonschema:"RFC 3339 time; empty clears it; omitted keeps it"`
	End          *string `json:"end,omitempty" jsonschema:"RFC 3339 time; empty clears it; omitted keeps it"`
	TimeZone     *string `json:"time_zone,omitempty" jsonschema:"IANA time zone; omitted keeps it"`
	LocalMessage *string `json:"local_message,omitempty" jsonschema:"plain text for people in the organisation; omitted keeps it"`
}

// setOutOfOffice changes the reply for people inside the organisation. It
// never touches the remote reply, and may not turn the reply on while the
// remote one would go out with it.
func (t mcpTools) setOutOfOffice(ctx context.Context, _ *mcp.CallToolRequest, in setOOOIn) (*mcp.CallToolResult, oooAgentView, error) {
	if !t.caller.canOrganize() {
		return nil, oooAgentView{}, errors.New(msgReadOnly)
	}
	if ok, _ := t.s.accountWriteLimiter.allow(t.caller.user.ID); !ok {
		return nil, oooAgentView{}, errors.New(msgSlowDown)
	}
	saved, err := t.s.saveOutOfOffice(t.caller.user.ID, func(prev config.OutOfOffice) (config.OutOfOffice, error) {
		if in.Enabled && prev.RemoteMode() != config.RemoteOff {
			return prev, errors.New(msgRemoteOOO)
		}
		next := prev
		next.Enabled = in.Enabled
		for dst, src := range map[*string]*string{&next.Start: in.Start, &next.End: in.End, &next.TimeZone: in.TimeZone, &next.LocalMessage: in.LocalMessage} {
			if src != nil {
				*dst = *src
			}
		}
		return next, nil
	})
	if err != nil {
		if errors.Is(err, errOOOInput) {
			return nil, oooAgentView{}, err
		}
		return nil, oooAgentView{}, errors.New("settings unavailable")
	}
	t.s.syncOOOCalendar(ctx, t.caller.user.ID, saved)
	t.s.syncOOOMessages(ctx, t.caller.user.ID, saved)
	t.log("out_of_office", "1", "0")
	return nil, t.oooAgentView(saved), nil
}
