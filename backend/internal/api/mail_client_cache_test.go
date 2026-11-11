package api

import (
	"fmt"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
)

type closingClient struct {
	imapadapter.Client
	closed bool
}

func (c *closingClient) Close() error { c.closed = true; return nil }

func TestMailClientCacheIsBoundedAndClosesEvicted(t *testing.T) {
	s := &Server{userMail: map[string]*serverMailEntry{}}
	clients := map[string]*closingClient{}
	for i := range maxRetainedMailClients + 10 {
		key := fmt.Sprint("user-", i)
		clients[key] = &closingClient{}
		s.retainMailClient(key, &serverMailEntry{client: clients[key]})
		if len(s.userMail) > maxRetainedMailClients {
			t.Fatal("cache exceeded its bound", len(s.userMail))
		}
	}
	closed := 0
	for key, c := range clients {
		_, retained := s.userMail[key]
		if c.closed == retained {
			t.Fatal("retained client closed or evicted client left open", key)
		}
		if c.closed {
			closed++
		}
	}
	if closed != 10 {
		t.Fatal("expected 10 evicted clients closed", closed)
	}
	// Replacing an existing key never evicts another entry.
	before := len(s.userMail)
	for key := range s.userMail {
		s.retainMailClient(key, &serverMailEntry{client: &closingClient{}})
		break
	}
	if len(s.userMail) != before {
		t.Fatal("replacement changed the cache size")
	}
}
