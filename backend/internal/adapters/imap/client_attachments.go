// Attachment enumeration and fetch, plus the size accounting the fetch
// partitioning depends on.
package imap

import (
	"context"
	"fmt"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"

	goimap "github.com/BrianLeishman/go-imap"
)

func emailContentSize(e *goimap.Email) int64 {
	total := int64(len(e.HTML)) + int64(len(e.Text))
	for _, a := range e.Attachments {
		total += int64(len(a.Content))
	}
	return total
}

// fetchAttachments returns one message's attachments through the same
// enmime parse native mail uses (ParseRawContent), so both backends list the
// same parts, including undisposed calendar invites go-imap's parser drops.
// FetchRawMessage refuses an oversized message with a UID LARGER search before
// any byte of it is fetched; ParseRawContent re-checks the decoded size.
func (c *APIClient) fetchAttachments(ctx context.Context, mailbox string, uid int) ([]mailmsg.Attachment, error) {
	if uid <= 0 {
		return nil, fmt.Errorf("invalid message id %d", uid)
	}
	raw, err := c.FetchRawMessage(ctx, strings.TrimSpace(mailbox), uid)
	if err != nil {
		return nil, err
	}
	parsed, err := ParseRawContent(raw)
	if err != nil {
		return nil, err
	}
	return parsed.Attachments, nil
}

func (c *APIClient) ListAttachments(ctx context.Context, mailbox string, uid int) ([]AttachmentInfo, error) {
	attachments, err := c.fetchAttachments(ctx, mailbox, uid)
	if err != nil {
		return nil, err
	}
	infos := make([]AttachmentInfo, 0, len(attachments))
	for i, a := range attachments {
		infos = append(infos, NewAttachmentInfo(i, a))
	}
	return infos, nil
}

// GetAttachment returns one attachment's content by index. The
// mailmsg.MaxInboundMessageBytes cap is enforced by fetchAttachments (on the
// whole message's total content, before any attachment is picked out here),
// so a request for a single attachment from an oversized message fails with
// mailmsg.ErrMessageTooLarge just as ListAttachments does.
func (c *APIClient) GetAttachment(ctx context.Context, mailbox string, uid int, index int) (AttachmentInfo, []byte, error) {
	attachments, err := c.fetchAttachments(ctx, mailbox, uid)
	if err != nil {
		return AttachmentInfo{}, nil, err
	}
	if index < 0 || index >= len(attachments) {
		return AttachmentInfo{}, nil, ErrAttachmentNotFound
	}
	return NewAttachmentInfo(index, attachments[index]), attachments[index].Content, nil
}
