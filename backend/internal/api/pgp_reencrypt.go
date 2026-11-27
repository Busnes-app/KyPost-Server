package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const (
	defaultReencryptPageLimit = 25
	maxReencryptPageLimit     = 100
	maxReencryptInspectBudget = 200
)

type pgpReencryptCandidate struct {
	Folder    string `json:"folder"`
	MessageID int    `json:"messageId"`
	SHA256    string `json:"sha256"`
	SizeBytes int    `json:"sizeBytes"`
}

type pgpReencryptCursor struct {
	Folder   string `json:"folder"`
	AfterUID int    `json:"afterUid"`
}

type pgpReencryptCandidatesResponse struct {
	Candidates    []pgpReencryptCandidate `json:"candidates"`
	NextCursor    *pgpReencryptCursor     `json:"nextCursor,omitempty"`
	Done          bool                    `json:"done"`
	V6PublicKey   string                  `json:"v6PublicKey"`
	V6Fingerprint string                  `json:"v6Fingerprint"`
}

type pgpReencryptRawResponse struct {
	Folder            string `json:"folder"`
	MessageID         int    `json:"messageId"`
	SHA256            string `json:"sha256"`
	ArmoredCiphertext string `json:"armoredCiphertext"`
}

type pgpReencryptReplaceRequest struct {
	Folder            string `json:"folder"`
	MessageID         int    `json:"messageId"`
	ExpectedSHA256    string `json:"expectedSha256"`
	ArmoredCiphertext string `json:"armoredCiphertext"`
}

type pgpReencryptReplaceResponse struct {
	Folder       string `json:"folder"`
	OldMessageID int    `json:"oldMessageId"`
	NewMessageID int    `json:"newMessageId"`
	SHA256       string `json:"sha256"`
}

func (s *Server) archiveReencryptSetup(w http.ResponseWriter, r *http.Request) (users.User, pgpmail.AccountArchiveKeys, imapadapter.ArchiveReencryptClient, bool) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
	}
	if refuseOtherSession(w, r, ac) {
		return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "user unavailable"})
		return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
	}
	if u.PGPProtection() != users.PGPProtectionClient {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "archive re-encryption requires client-protected PGP keys"})
		return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
	}
	if !s.pgpV6Ready(u) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "post-quantum v6 is not active yet"})
		return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
	}
	var ringFPs []string
	if u.PGPKeyring != nil {
		ringFPs = u.PGPKeyring.KeyFingerprints
	}
	keys, err := pgpmail.ArchiveKeysForAccount(u.PGPPublicKey, u.PGPPublicKeyV6, ringFPs)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "v6 certificate unavailable for archive re-encryption"})
		return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
	}
	mailClient, err := s.mailFor(r)
	if err != nil {
		if writeMailSourceConflict(w, err) {
			return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
		}
		if errors.Is(err, errIMAPNotConfigured) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "imap configuration is required"})
			return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "mail backend unavailable"})
		return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
	}
	reClient, ok := mailClient.(imapadapter.ArchiveReencryptClient)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "mail backend does not support archive re-encryption"})
		return users.User{}, pgpmail.AccountArchiveKeys{}, nil, false
	}
	return u, keys, reClient, true
}

func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// handlePGPReencryptCandidates lists stored messages across all folders whose
// OpenPGP PKESKs still address the account's v4 key(s) rather than its active
// v6 encryption subkey.
//
// GET /api/mail/pgp-reencrypt/candidates?folder=&afterUid=0&limit=25
func (s *Server) handlePGPReencryptCandidates(w http.ResponseWriter, r *http.Request) {
	u, keys, reClient, ok := s.archiveReencryptSetup(w, r)
	if !ok {
		return
	}

	limit := defaultReencryptPageLimit
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > maxReencryptPageLimit {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid limit"})
			return
		}
		limit = parsed
	}
	startFolder := strings.TrimSpace(r.URL.Query().Get("folder"))
	afterUID := 0
	if rawAfter := strings.TrimSpace(r.URL.Query().Get("afterUid")); rawAfter != "" {
		parsed, err := strconv.Atoi(rawAfter)
		if err != nil || parsed < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid afterUid"})
			return
		}
		afterUID = parsed
	}

	folders, err := reClient.ListAllFolders(r.Context())
	if err != nil {
		s.logger.Error("pgp archive candidate folder list failed", "user_id", u.ID, "error", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to list folders"})
		return
	}

	candidates := make([]pgpReencryptCandidate, 0, limit)
	inspected := 0
	var nextCursor *pgpReencryptCursor
	done := true

	for idx, folder := range folders {
		if startFolder != "" {
			if folder < startFolder {
				continue
			}
		}
		cursorUID := 0
		if folder == startFolder {
			cursorUID = afterUID
		}

		for {
			remainingInspect := maxReencryptInspectBudget - inspected
			if remainingInspect <= 0 {
				done = false
				nextCursor = &pgpReencryptCursor{Folder: folder, AfterUID: cursorUID}
				goto respond
			}
			batchSize := remainingInspect
			if batchSize > 50 {
				batchSize = 50
			}
			uids, err := reClient.ListFolderMessageUIDs(r.Context(), folder, cursorUID, batchSize)
			if err != nil {
				s.logger.Error("pgp archive candidate uid scan failed", "user_id", u.ID, "folder", folder, "error", err.Error())
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to scan folder"})
				return
			}
			if len(uids) == 0 {
				break
			}
			for _, uid := range uids {
				cursorUID = uid
				inspected++
				raw, err := reClient.GetRawMessage(r.Context(), folder, uid)
				if err != nil {
					if errors.Is(err, imapadapter.ErrMessageNotFound) {
						continue
					}
					s.logger.Error("pgp archive candidate raw fetch failed", "user_id", u.ID, "folder", folder, "uid", strconv.Itoa(uid), "error", err.Error())
					continue
				}
				if pgpmail.IsV4ArchiveCandidate(raw, keys) {
					candidates = append(candidates, pgpReencryptCandidate{
						Folder:    folder,
						MessageID: uid,
						SHA256:    sha256HexBytes(raw),
						SizeBytes: len(raw),
					})
					if len(candidates) >= limit {
						done = false
						nextCursor = &pgpReencryptCursor{Folder: folder, AfterUID: cursorUID}
						goto respond
					}
				}
				if inspected >= maxReencryptInspectBudget {
					done = false
					nextCursor = &pgpReencryptCursor{Folder: folder, AfterUID: cursorUID}
					goto respond
				}
			}
			if len(uids) < batchSize {
				break
			}
		}
		// Advance past this folder; if another folder follows and we hit budget
		// on the next iteration, cursor resets cleanly at AfterUID=0.
		_ = idx
	}

respond:
	writeJSON(w, http.StatusOK, pgpReencryptCandidatesResponse{
		Candidates:    candidates,
		NextCursor:    nextCursor,
		Done:          done,
		V6PublicKey:   u.PGPPublicKeyV6,
		V6Fingerprint: keys.V6PrimaryFingerprint,
	})
}

// handlePGPReencryptRaw returns the armored OpenPGP ciphertext and exact
// stored raw-message SHA-256 (CAS token) for one v4 archive candidate.
//
// GET /api/mail/pgp-reencrypt/raw?folder=<folder>&messageId=<uid>
func (s *Server) handlePGPReencryptRaw(w http.ResponseWriter, r *http.Request) {
	u, keys, reClient, ok := s.archiveReencryptSetup(w, r)
	if !ok {
		return
	}

	folder := strings.TrimSpace(r.URL.Query().Get("folder"))
	uid, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("messageId")))
	if folder == "" || err != nil || uid <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "folder and positive messageId are required"})
		return
	}

	raw, err := reClient.GetRawMessage(r.Context(), folder, uid)
	if err != nil {
		if errors.Is(err, imapadapter.ErrMessageNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "message not found"})
			return
		}
		s.logger.Error("pgp archive raw fetch failed", "user_id", u.ID, "folder", folder, "uid", strconv.Itoa(uid), "error", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to fetch message"})
		return
	}
	if int64(len(raw)) > mailmsg.MaxInboundMessageBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "message exceeds maximum size"})
		return
	}
	if !pgpmail.IsV4ArchiveCandidate(raw, keys) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "message is not a v4 archive re-encryption candidate"})
		return
	}
	armored, err := pgpmail.ExtractPGPMIMEEncryptedPayload(raw)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "message does not contain a valid PGP/MIME encrypted payload"})
		return
	}

	writeJSON(w, http.StatusOK, pgpReencryptRawResponse{
		Folder:            folder,
		MessageID:         uid,
		SHA256:            sha256HexBytes(raw),
		ArmoredCiphertext: armored,
	})
}

// handlePGPReencryptReplace validates a client-produced v6 replacement
// ciphertext and atomically replaces the stored message using compare-and-swap
// against expectedSha256 while preserving outer RFC 5322 headers, folder,
// flags, and (on native storage) message ID.
//
// POST /api/mail/pgp-reencrypt/replace
func (s *Server) handlePGPReencryptReplace(w http.ResponseWriter, r *http.Request) {
	u, keys, reClient, ok := s.archiveReencryptSetup(w, r)
	if !ok {
		return
	}

	var req pgpReencryptReplaceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxClientCiphertextBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	req.Folder = strings.TrimSpace(req.Folder)
	req.ExpectedSHA256 = strings.ToLower(strings.TrimSpace(req.ExpectedSHA256))
	if req.Folder == "" || req.MessageID <= 0 || len(req.ExpectedSHA256) != 64 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "folder, positive messageId, and 64-hex expectedSha256 are required"})
		return
	}
	if _, err := hex.DecodeString(req.ExpectedSHA256); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expectedSha256 must be valid hex"})
		return
	}

	// Security Property P6: strictly verify replacement PKESK names ONLY the
	// account's current v6 ML-KEM-768+X25519 encryption subkey + SEIPDv2 AEAD.
	if err := pgpmail.ValidateV6ArchiveCiphertext(req.ArmoredCiphertext, keys); err != nil {
		s.logger.Info("pgp archive replacement rejected", "user_id", u.ID, "folder", req.Folder, "uid", strconv.Itoa(req.MessageID), "reason", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	oldRaw, err := reClient.GetRawMessage(r.Context(), req.Folder, req.MessageID)
	if err != nil {
		if errors.Is(err, imapadapter.ErrMessageNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "message not found"})
			return
		}
		s.logger.Error("pgp archive replace pre-read failed", "user_id", u.ID, "folder", req.Folder, "uid", strconv.Itoa(req.MessageID), "error", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to fetch stored message"})
		return
	}
	if !strings.EqualFold(sha256HexBytes(oldRaw), req.ExpectedSHA256) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "stored message changed before replacement"})
		return
	}
	if !pgpmail.IsV4ArchiveCandidate(oldRaw, keys) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "stored message is no longer a v4 archive candidate"})
		return
	}

	newRaw, err := pgpmail.RebuildPGPMIMEMessage(oldRaw, req.ArmoredCiphertext)
	if err != nil {
		if errors.Is(err, mailmsg.ErrMessageTooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "re-encrypted message exceeds maximum message size"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	newUID, err := reClient.ReplaceRawMessageCAS(r.Context(), req.Folder, req.MessageID, req.ExpectedSHA256, newRaw)
	if err != nil {
		switch {
		case errors.Is(err, imapadapter.ErrCASMismatch):
			writeJSON(w, http.StatusConflict, map[string]any{"error": "stored message changed before replacement"})
		case errors.Is(err, imapadapter.ErrMessageNotFound):
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "message not found"})
		case errors.Is(err, mailmsg.ErrMessageTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "re-encrypted message exceeds maximum message size"})
		case errors.Is(err, mailbox.ErrCapacity), errors.Is(err, mailbox.ErrPoolFull), errors.Is(err, fsutil.ErrDriveReserve):
			writeJSON(w, http.StatusInsufficientStorage, map[string]any{"error": "mailbox storage capacity exceeded"})
		default:
			s.logger.Error("pgp archive replace failed", "user_id", u.ID, "folder", req.Folder, "uid", strconv.Itoa(req.MessageID), "error", err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to replace stored message"})
		}
		return
	}

	if newUID != req.MessageID {
		if cache, cacheErr := s.mailCacheFor(r); cacheErr == nil && cache != nil {
			cache.Remove(req.Folder, req.MessageID)
		}
	}

	s.logger.Info("pgp archive message re-encrypted to v6", "user_id", u.ID, "folder", req.Folder, "uid", strconv.Itoa(newUID))
	writeJSON(w, http.StatusOK, pgpReencryptReplaceResponse{
		Folder:       req.Folder,
		OldMessageID: req.MessageID,
		NewMessageID: newUID,
		SHA256:       sha256HexBytes(newRaw),
	})
}
