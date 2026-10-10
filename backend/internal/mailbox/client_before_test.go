package mailbox

import (
	"context"
	"fmt"
	"testing"
)

// before= paging walks strictly older mail, newest first, and says when the
// mailbox has nothing older left.
func TestNativeClientListOverviewsBefore(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	var uids []int
	for i := 0; i < 5; i++ {
		raw := []byte(fmt.Sprintf("From: s@outside.test\r\nSubject: m%d\r\n\r\nbody", i))
		uids = append(uids, importClient(t, s, fmt.Sprintf("m%d", i), raw))
	}
	page, more, err := c.ListOverviewsBefore(ctx, "INBOX", uids[4], 2)
	must(t, err)
	if !more || len(page) != 2 || page[0].UID != uids[3] || page[1].UID != uids[2] {
		t.Fatalf("first page = %+v more=%v, want uids %d,%d and more", page, more, uids[3], uids[2])
	}
	page, more, err = c.ListOverviewsBefore(ctx, "INBOX", page[1].UID, 2)
	must(t, err)
	if more || len(page) != 2 || page[0].UID != uids[1] || page[1].UID != uids[0] {
		t.Fatalf("last page = %+v more=%v, want uids %d,%d and no more", page, more, uids[1], uids[0])
	}
	if page, more, err = c.ListOverviewsBefore(ctx, "INBOX", 1, 2); err != nil || more || len(page) != 0 {
		t.Fatalf("before=1 = %+v %v %v, want empty", page, more, err)
	}
}
