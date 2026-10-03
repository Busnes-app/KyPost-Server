package imap

// MailSourceIdentity keeps legacy IMAP's existing configuration behavior while
// letting native storage expose its immutable owner/database namespace. It is
// an account-mode fence; IMAP endpoint/UIDVALIDITY migration is separate work.
func MailSourceIdentity(client Client) string {
	if native, ok := client.(interface{ MailSourceIdentity() string }); ok {
		return native.MailSourceIdentity()
	}
	return "imap"
}
