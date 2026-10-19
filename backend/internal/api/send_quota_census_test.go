package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// sendSinks deliver mail. None of them knows the sending user, so the quota
// is charged by their callers instead (admitSend, chargeSend).
var sendSinks = map[string]bool{
	"SMTPDeliver": true, "SMTPDeliverPrepared": true, "Deliver": true, "NativeOutbound": true,
	"nativeOutbound": true, "finishMailSend": true, "finishNativeSend": true, "deliverIMAP": true,
	"sendPickupNotifications": true, "sendPickupNotification": true,
}

// sendCallers is every package.function (or package-level var) allowed to
// name a sink, each with why the quota holds there. A new one fails the test
// until it is charged too.
var sendCallers = map[string]string{
	"api.handleMailSend":            "admitSend",
	"api.handleMailSendPGP":         "admitSend",
	"api.handleSendAsCreate":        "admitSend; the per-address cooldown alone resets on delete and re-add",
	"api.dispatchScheduled":         "chargeSend",
	"api.sendITIP":                  "chargeSend, keyed by the KyCalendar event ID",
	"api.finishMailSend":            "a sink, reached only from handleMailSend",
	"api.finishNativeSend":          "a sink, reached only from the two send handlers",
	"api.deliverIMAP":               "a sink, reached only from handleMailSendPGP and dispatchScheduled",
	"api.sendPickupNotifications":   "a sink; its recipients were charged with the message",
	"api.sendPickupNotification":    "a sink, reached only from sendPickupNotifications",
	"api.nativeOutbound":            "a sink, reached only from the callers listed here",
	"api.handleNativeOutboxStatus":  "reads outbox status; sends nothing",
	"api.sendAdminNotice":           "system mail to the admin's own address",
	"processor.sendRejectionNotice": "notice to the account's own address",
	"processor.sendSendAsProbe":     "probe to the account's own address",
	"sso.deliver":                   "delivers outbox jobs charged when api admitted them",
	"app.startNativeOutbound":       "recovers pending outbox jobs; queues none",
	"mailmsg.SMTPDeliverPrepared":   "a sink",
}

// TestEverySendPathChargesTheQuota fails on a send sink named anywhere in
// internal/ outside sendCallers, the way a guard at call sites has gone
// missing here before. Names, not only calls, so a sink held in a var counts.
func TestEverySendPathChargesTheQuota(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		check := func(scope string, n ast.Node) {
			ast.Inspect(n, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if ok && sendSinks[id.Name] && sendCallers[file.Name.Name+"."+scope] == "" {
					t.Errorf("%s: %s.%s names %s without being in sendCallers; charge the sending quota there, then list it", fset.Position(id.Pos()), file.Name.Name, scope, id.Name)
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Body != nil {
					check(decl.Name.Name, decl.Body)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if v, ok := spec.(*ast.ValueSpec); ok {
						for _, value := range v.Values {
							check(v.Names[0].Name, value)
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
