package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The orgAdmit call-site scan (tenancy design, "The cross-user hops"). It finds every call
// in api/ that opens a user's store or reads the users store by an ID, and fails unless
// the ID came from the caller's own AuthContext, the function goes through the org gate,
// or orgAdmitExempt says why the call is not a cross-user hop.
//
// A function that passes its own parameter on as that ID becomes an accessor itself, so
// the check moves to its callers. Closures count as their enclosing function.

// orgAccessors is each base accessor and the index of its ID argument.
var orgAccessors = map[string]int{
	"users.Get": 0, "users.GetBySSOSubIssuer": 1, "users.GetBySSOSub": 0, "users.GetByUsername": 0,
	"userStore": 0, "userStoreForMaintenance": 0, "withUserStoreForRevocation": 0,
	"userContactsStore": 0, "userSendAsStore": 0, "userGroupsStore": 0, "userRulesStore": 0,
	"userMailCacheStore": 0, "userMailClient": 0, "mailboxMailClient": 0,
	"nativeMailAssignment": 1, "nativeMailboxAssignment": 1,
}

// orgGates are the functions that make the org check for a hop.
var orgGates = []string{"orgAdmit", "peerOrgAdmit", "sameOrgPeople", "addressOneOrg", "quarantineOneOrg", "checkAccountOrg", "bindDirectoryOrg"}

// authSources return the caller's own AuthContext; ownIDSources return, first, the ID of
// the account whose credential the request presented.
var (
	authSources  = []string{"authFromContext", "authContextFromContext", "currentUser", "resolveMailAuthContext", "agentTokenOwner", "userAndStore"}
	ownIDSources = []string{"deviceAuthFromRequest", "deviceAuthSnapshot"}
)

// orgAdmitExempt is every function that reaches an accessor with an ID not from the
// caller's AuthContext and is not a cross-user hop. Each needs a reason a reviewer can
// check; adding one is not a way to green the scan.
var orgAdmitExempt = map[string]string{
	// Authentication: the credential names the account, which becomes the caller.
	"currentUser":                      orgExemptAuthN + " (the session's account; it sets AuthContext.OrgID)",
	"mintSession":                      orgExemptAuthN + " (the session just minted for its account)",
	"deviceAuthSnapshot":               orgExemptAuthN + " (the paired device's owner; refuses an org-less account in multi mode)",
	"withDAVBasicAuth":                 orgExemptAuthN + " (the CardDAV password's account; sets OrgID)",
	"withMCPAuth":                      orgExemptAuthN + " (the agent token's account; sets OrgID)",
	"handleLogin":                      orgExemptAuthN + " (the password's account)",
	"handleLoginParams":                "pre-authentication salt lookup by username; it answers an unknown name the same way and reads no store",
	"handleMFATOTP":                    orgExemptAuthN + " (the MFA challenge's account)",
	"handleMFARecoveryCode":            orgExemptAuthN + " (the MFA challenge's account)",
	"handlePushFinish":                 orgExemptAuthN + " (the approved push challenge's account)",
	"handleReviewPairing":              orgExemptAuthN + " (the password's account)",
	"admitNativeSignOn":                orgExemptAuthN + " (the device sign-on token's account, org-checked by resolveSSOUser, PR 2)",
	"linkSSOIdentity":                  "SSO link; it answers 404 in multi mode (PR 2) before this lookup",
	"handleSSOBackchannelLogout":       "logoutOrgStatus is PR 2's org check for the logout token's sub",
	"handleNotificationNativeRegister": orgExemptAuthN + " (the pairing token's subscriber's owner)",
	"handleNativePairingStatus":        orgExemptAuthN + " (the pending device's own secret)",
	"handlePGPQRKey":                   orgExemptSigned + " (the QR pairing token's subject)",
	"handleKyPulseConfirm":             orgExemptSigned + " (the confirmation link's account)",
	// The caller's own account, reached in a way the scan does not follow.
	"handlePGPIdentityClient": "u is the record SetPGPIdentityClientProtected returns for ac.UserID",
	"handleImportUpload":      "g is an import grant importGrantOf admits only for ac.UserID",
	"handleIMAPImportRun":     "g is an import grant importGrantOf admits only for ac.UserID",
	"importOne":               "job.user is the import grant's account, the caller's own",
	"lookupPublishedKey":      "public WKD key lookup by address; publishing a key is its owner's choice, not an org-bound read",
	"sendAdminNotice":         "the install's first administrator notifies itself; no request names the account",
	// Workers: no request; each works from the account a stored row or the ledger names.
	"dispatchScheduled":             orgExemptWorker + " (a scheduled send runs as the user that queued it)",
	"saveScheduledDraft":            orgExemptWorker + " (a scheduled send runs as the user that queued it)",
	"scheduledAuthority":            orgExemptWorker + " (a scheduled send runs as the user that queued it)",
	"scheduledSMTPAccount":          orgExemptWorker + " (a scheduled send runs as the user that queued it)",
	"StartContactPhotoSweeper":      orgExemptWorker,
	"StartContactsTombstoneSweeper": orgExemptWorker,
	"sweepDeviceIndex":              orgExemptWorker,
	"rescanDeviceIndex":             orgExemptWorker,
	"rescanSubscriberIndex":         orgExemptWorker,
	"resyncOOOCalendars":            orgExemptWorker,
	"StartNativeProvisioning":       orgExemptWorker + " (the ledger's subjects)",
	"deactivateAndRevoke":           "revokes the account a signed, org-checked directory event (bindDirectoryOrg) deactivates",
	// Peers.
	"handleHRMailbox": "answers 404 in multi mode before any lookup (K6)",
	// Administrator routes over any account: staff org selection and its orgAdmit are build
	// plan PR 4 (route classes, X-Ky-Org). K7 already limits administrators to staff.
	"handleAdminUserCardDAVClient": orgExemptAdmin,
	"handleAdminUserIMAPConfig":    orgExemptAdmin,
	"handleUserKeySetup":           orgExemptAdmin,
	"handleUsersResetPassword":     orgExemptAdmin,
	"handleUsersClearMFA":          orgExemptAdmin,
	"handleUsersDeactivate":        orgExemptAdmin,
	"handleNativeMailAddresses":    orgExemptAdmin,
	"handleNativeMailboxes":        orgExemptAdmin,
	"handleNativeRecoveryRepair":   orgExemptAdmin,
	"setNativeMailboxState":        orgExemptAdmin,
}

const (
	orgExemptAuthN  = "authenticates a credential; the account it names becomes the caller, so there is no second user"
	orgExemptSigned = "a link or token KyPost signed for one account names it"
	orgExemptWorker = "background worker, not a request hop; workers check org status in PR 5 (K Background workers)"
	orgExemptAdmin  = "administrator route; staff org selection and its orgAdmit are build plan PR 4"
)

type orgScanFunc struct {
	name       string
	params     []string
	authParams []string // parameters typed AuthContext
	body       *ast.BlockStmt
	// gateArgs are the texts of every argument passed to an org gate in the function; an
	// accessor call is admitted only when its ID or its result is one of them.
	gateArgs map[string]bool
	lhsOf    map[*ast.CallExpr]string // the variable an accessor call's result is assigned to
}

// orgScan returns "function: accessor(arg)" for every unadmitted call in files.
func orgScan(files []*ast.File, exempt map[string]string) []string {
	var funcs []*orgScanFunc
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			sf := &orgScanFunc{name: fd.Name.Name, body: fd.Body}
			for _, field := range fd.Type.Params.List {
				for _, n := range field.Names {
					sf.params = append(sf.params, n.Name)
					if id, ok := field.Type.(*ast.Ident); ok && id.Name == "AuthContext" {
						sf.authParams = append(sf.authParams, n.Name)
					}
				}
			}
			sf.gateArgs, sf.lhsOf = map[string]bool{}, map[*ast.CallExpr]string{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if slices.Contains(orgGates, callName(x)) {
						for _, a := range x.Args {
							sf.gateArgs[exprText(a)] = true
						}
					}
				case *ast.AssignStmt:
					if c, ok := x.Rhs[0].(*ast.CallExpr); ok && len(x.Rhs) == 1 {
						if id, ok := x.Lhs[0].(*ast.Ident); ok {
							sf.lhsOf[c] = id.Name
						}
					}
				}
				return true
			})
			funcs = append(funcs, sf)
		}
	}
	accessors := map[string]int{}
	for k, v := range orgAccessors {
		accessors[k] = v
	}
	for changed := true; changed; {
		changed = false
		for _, f := range funcs {
			for _, c := range orgCalls(f, accessors) {
				if i := slices.Index(f.params, c.arg); c.kind == argParam && i >= 0 {
					if _, done := accessors[f.name]; !done {
						accessors[f.name], changed = i, true
					}
				}
			}
		}
	}
	var out []string
	for _, f := range funcs {
		if exempt[f.name] != "" {
			continue
		}
		for _, c := range orgCalls(f, accessors) {
			base, _, _ := strings.Cut(c.arg, ".")
			if c.kind == argOther && !f.gateArgs[c.arg] && !f.gateArgs[base] && !f.gateArgs[c.lhs] {
				out = append(out, f.name+": "+c.accessor+"("+c.arg+")")
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

const (
	argOwn = iota
	argParam
	argOther
)

type orgCall struct {
	accessor, arg, lhs string
	kind               int
}

// orgCalls classifies each accessor call's ID argument in f. Own: X.UserID of an
// AuthContext, or X.ID of a user record read by an own ID. Param: a parameter of f.
// orgScan then admits an "other" ID when it, its base variable or the call's result is
// passed to an org gate in f; a gate elsewhere in f admits nothing else.
func orgCalls(f *orgScanFunc, accessors map[string]int) []orgCall {
	authVars, ownUsers, ownIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	paramUsers := map[string]string{} // a user record read by a parameter, to that parameter
	for _, p := range f.authParams {
		authVars[p] = true
	}
	var out []orgCall
	classify := func(e ast.Expr) (string, int) {
		text := exprText(e)
		// The MCP caller is the agent token's own account (withMCPAuth).
		if strings.HasSuffix(text, ".caller.user.ID") {
			return text, argOwn
		}
		switch x := e.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			switch {
			case !ok:
			case x.Sel.Name == "UserID" && authVars[id.Name], x.Sel.Name == "ID" && ownUsers[id.Name]:
				return text, argOwn
			case x.Sel.Name == "ID" && slices.Contains(f.params, id.Name):
				return id.Name, argParam // a users.User parameter's ID
			case x.Sel.Name == "ID" && paramUsers[id.Name] != "":
				return paramUsers[id.Name], argParam
			}
		case *ast.Ident:
			switch {
			case ownIDs[x.Name], ownUsers[x.Name]:
				return text, argOwn
			case slices.Contains(f.params, x.Name):
				return text, argParam
			case paramUsers[x.Name] != "":
				return paramUsers[x.Name], argParam
			}
		}
		return text, argOther
	}
	ast.Inspect(f.body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Rhs) == 1 && len(x.Lhs) > 0 {
				if c, ok := x.Rhs[0].(*ast.CallExpr); ok {
					lhs, _ := x.Lhs[0].(*ast.Ident)
					name := callName(c)
					if lhs != nil && slices.Contains(authSources, name) {
						authVars[lhs.Name] = true
					}
					if lhs != nil && slices.Contains(ownIDSources, name) {
						ownIDs[lhs.Name] = true
					}
					if i, found := accessors[name]; found && lhs != nil && strings.HasPrefix(name, "users.") && i < len(c.Args) {
						switch arg, kind := classify(c.Args[i]); kind {
						case argOwn:
							ownUsers[lhs.Name] = true
						case argParam:
							paramUsers[lhs.Name] = arg
						}
					}
				}
			}
		case *ast.FuncLit:
			for _, field := range x.Type.Params.List {
				if sel, ok := field.Type.(*ast.Ident); ok && sel.Name == "AuthContext" {
					for _, n := range field.Names {
						authVars[n.Name] = true
					}
				}
			}
		case *ast.CallExpr:
			name := callName(x)
			if i, ok := accessors[name]; ok && i < len(x.Args) && name != f.name {
				arg, kind := classify(x.Args[i])
				out = append(out, orgCall{accessor: name, arg: arg, lhs: f.lhsOf[x], kind: kind})
			}
		}
		return true
	})
	return out
}

// callName is "users.Get" for x.users.Get(...), the method or function name otherwise.
func callName(c *ast.CallExpr) string {
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if inner, ok := fn.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "users" {
			return "users." + fn.Sel.Name
		}
		return fn.Sel.Name
	}
	return ""
}

func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return exprText(x.Fun) + "(…)"
	case *ast.IndexExpr:
		return exprText(x.X) + "[…]"
	case *ast.BasicLit:
		return x.Value
	case *ast.UnaryExpr:
		return exprText(x.X)
	case *ast.StarExpr:
		return exprText(x.X)
	}
	return "…"
}

func parseAPIForOrgScan(t *testing.T) []*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// TestOrgAdmitCallSites fails the build when a function reaches another user's store, or
// the users store, by an ID that did not come from the caller's AuthContext, without the
// org gate or a written exemption.
func TestOrgAdmitCallSites(t *testing.T) {
	files := parseAPIForOrgScan(t)
	for _, v := range orgScan(files, orgAdmitExempt) {
		t.Errorf("unadmitted cross-user hop %s: call orgAdmit (or another org gate), or add the function to orgAdmitExempt with the reason it is not a hop", v)
	}
	names := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				names[fd.Name.Name] = true
			}
		}
	}
	for name, why := range orgAdmitExempt {
		if !names[name] || strings.TrimSpace(why) == "" {
			t.Errorf("orgAdmitExempt[%q] names no function in api/ or has no reason", name)
		}
	}
}

// The scan refuses a handler that reads another user's record by a path ID without
// orgAdmit, follows a helper's parameter to its caller, admits the gated version, and
// still refuses a second, ungated hop in a function that gates the first.
func TestOrgAdmitScanFailsAnUngatedHandler(t *testing.T) {
	const src = `package api
func (s *Server) handleFixture(w http.ResponseWriter, r *http.Request) {
	u, _ := s.users.Get(r.PathValue("id"))
	_ = u
}
func (s *Server) handleFixtureViaHelper(w http.ResponseWriter, r *http.Request) {
	s.fixtureHelper(r.PathValue("id"))
}
func (s *Server) fixtureHelper(userID string) { _, _ = s.userStore(userID) }
func (s *Server) handleFixtureOwn(w http.ResponseWriter, r *http.Request) {
	ac, _ := authFromContext(r)
	u, _ := s.users.Get(ac.UserID)
	s.fixtureHelper(u.ID)
}
func (s *Server) handleFixtureGated(w http.ResponseWriter, r *http.Request) {
	ac, _ := authFromContext(r)
	u, _ := s.users.Get(r.PathValue("id"))
	if !s.orgAdmit(ac, u) {
		return
	}
	_, _ = s.userStore(u.ID)
}
func (s *Server) handleFixtureGatedPlusHop(w http.ResponseWriter, r *http.Request) {
	ac, _ := authFromContext(r)
	u, _ := s.users.Get(r.PathValue("id"))
	if !s.orgAdmit(ac, u) {
		return
	}
	other, _ := s.users.Get(r.PathValue("other"))
	_ = other
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := orgScan([]*ast.File{f}, nil)
	want := []string{`handleFixture: users.Get(r.PathValue(…))`, `handleFixtureGatedPlusHop: users.Get(r.PathValue(…))`, `handleFixtureViaHelper: fixtureHelper(r.PathValue(…))`}
	if !slices.Equal(got, want) {
		t.Fatalf("scan found %q, want %q", got, want)
	}
	if got := orgScan([]*ast.File{f}, map[string]string{"handleFixture": "x", "handleFixtureGatedPlusHop": "x", "handleFixtureViaHelper": "x"}); len(got) != 0 {
		t.Fatalf("exempted functions still reported: %q", got)
	}
}
