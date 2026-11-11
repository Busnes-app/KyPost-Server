package api

import (
	"fmt"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
)

// closingClient records Close; release, when set, makes Close block on it.
type closingClient struct {
	imapadapter.Client
	closed  chan struct{}
	release chan struct{}
}

func newClosingClient() *closingClient { return &closingClient{closed: make(chan struct{})} }

func (c *closingClient) Close() error {
	if c.release != nil {
		<-c.release
	}
	close(c.closed)
	return nil
}

func isClosed(c *closingClient) bool {
	select {
	case <-c.closed:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

func TestMailClientCacheIsBoundedAndClosesEvicted(t *testing.T) {
	s := &Server{userMail: map[string]*serverMailEntry{}}
	clients := map[string]*closingClient{}
	for i := range maxRetainedMailClients + 10 {
		key := fmt.Sprint("user-", i)
		clients[key] = newClosingClient()
		s.retainMailClient(key, &serverMailEntry{client: clients[key]})
		if len(s.userMail) > maxRetainedMailClients {
			t.Fatal("cache exceeded its bound", len(s.userMail))
		}
	}
	closed := 0
	for key, c := range clients {
		if _, retained := s.userMail[key]; retained {
			select {
			case <-c.closed:
				t.Fatal("retained client closed", key)
			default:
			}
			continue
		}
		if !isClosed(c) {
			t.Fatal("evicted client left open", key)
		}
		closed++
	}
	if closed != 10 {
		t.Fatal("expected 10 evicted clients closed", closed)
	}
	// Replacing an existing key never evicts another entry.
	before := len(s.userMail)
	for key := range s.userMail {
		s.retainMailClient(key, &serverMailEntry{client: newClosingClient()})
		break
	}
	if len(s.userMail) != before {
		t.Fatal("replacement changed the cache size")
	}
}

// An evicted client whose Close blocks (another user's IMAP dial to a slow
// host) must not hold userMu, which every API request needs.
func TestMailClientEvictionDoesNotHoldUserMu(t *testing.T) {
	s := &Server{userMail: map[string]*serverMailEntry{}}
	release := make(chan struct{})
	defer close(release)
	for i := range maxRetainedMailClients {
		c := newClosingClient()
		c.release = release
		s.userMail[fmt.Sprint("user-", i)] = &serverMailEntry{client: c}
	}
	done := make(chan struct{})
	go func() {
		s.userMu.Lock()
		s.retainMailClient("new", &serverMailEntry{client: newClosingClient()})
		s.userMu.Unlock()
		s.userMu.Lock() // a later request on any user
		s.userMu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("eviction held userMu while Close blocked")
	}
}
