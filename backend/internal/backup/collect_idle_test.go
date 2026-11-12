package backup

import (
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// A restored idle mark could hide restored Sent obligations; never collect one.
func TestCollectSkipsOutboundIdleMark(t *testing.T) {
	for _, name := range []string{mailbox.OutboundIdleFile, mailbox.OutboundIdleFile + ".tmp", mailbox.OutboundIdleFile + ".lock"} {
		if !skip(name) {
			t.Error("backup collects", name)
		}
	}
}
