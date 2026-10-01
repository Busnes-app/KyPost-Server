package state

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// MaxSorterCorrections bounds the sorter's stored training data for one
// account, across all labels, oldest dropped first. A per-label cap would let
// the total grow with every label name ever used; the training budget is
// enforced again at read time (SorterCorrectionsStrict's limit, sorter.Train).
const MaxSorterCorrections = 1000

// SorterExample is one stored correction: the message's vector and the label
// the user gave it.
type SorterExample struct {
	Label string
	Vec   []float32
}

// SorterCheck is the guard both processes compute from the IMAP adapter's
// sender and subject strings for the same message. It is a hash so the sorter
// tables hold no correspondence text.
func SorterCheck(sender, subject string) string {
	sum := sha256.Sum256([]byte(sender + "\x00" + subject))
	return hex.EncodeToString(sum[:])
}

// RecordSorterPrediction remembers what the sorter made of a message, so a
// later keyword change can be recognised as a correction.
func (s *Store) RecordSorterPrediction(messageID, check, model string, vec []float32, predicted string) error {
	_, err := s.db.Exec(
		`INSERT INTO sorter_predictions(message_id, check_hash, model, vec, predicted, at_unix) VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(message_id) DO UPDATE SET check_hash = excluded.check_hash, model = excluded.model,
		   vec = excluded.vec, predicted = excluded.predicted, at_unix = excluded.at_unix`,
		messageID, check, model, encodeVec(vec), predicted, time.Now().Unix())
	return err
}

// SorterPrediction returns the recorded check and label for messageID.
func (s *Store) SorterPrediction(messageID string) (check, predicted string, found bool, err error) {
	err = s.db.QueryRow(`SELECT check_hash, predicted FROM sorter_predictions WHERE message_id = ?`, messageID).
		Scan(&check, &predicted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return check, predicted, err == nil, err
}

// RecordSorterCorrection records that the user filed messageID under label.
// It learns only when the message has a recorded prediction whose check
// matches; relabelling back to the sorter's own answer withdraws an earlier
// correction instead. Returns whether the training data changed.
func (s *Store) RecordSorterCorrection(messageID, check, label string) (bool, error) {
	changed := false
	err := s.tx(func(tx *sql.Tx) error {
		var (
			gotCheck, model, predicted string
			vec                        []byte
		)
		err := tx.QueryRow(`SELECT check_hash, model, vec, predicted FROM sorter_predictions WHERE message_id = ?`,
			messageID).Scan(&gotCheck, &model, &vec, &predicted)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && gotCheck != check) {
			return nil
		}
		if err != nil {
			return err
		}
		var res sql.Result
		if label == predicted {
			res, err = tx.Exec(`DELETE FROM sorter_corrections WHERE message_id = ?`, messageID)
		} else {
			res, err = tx.Exec(
				`INSERT INTO sorter_corrections(message_id, label, model, vec, at_unix) VALUES(?, ?, ?, ?, ?)
				 ON CONFLICT(message_id) DO UPDATE SET label = excluded.label, model = excluded.model,
				   vec = excluded.vec, at_unix = excluded.at_unix
				 WHERE sorter_corrections.label != excluded.label`,
				messageID, label, model, vec, time.Now().Unix())
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if _, err := tx.Exec(`DELETE FROM sorter_corrections WHERE message_id NOT IN (
			SELECT message_id FROM sorter_corrections ORDER BY at_unix DESC, rowid DESC LIMIT ?)`,
			MaxSorterCorrections); err != nil {
			return err
		}
		changed = true
		return bumpSorterGeneration(tx)
	})
	return changed, err
}

func bumpSorterGeneration(tx *sql.Tx) error {
	cur, err := metaString(tx, metaSorterGeneration)
	if err != nil {
		return err
	}
	n, _ := strconv.ParseInt(cur, 10, 64)
	return setMeta(tx, metaSorterGeneration, strconv.FormatInt(n+1, 10))
}

// SorterGeneration changes whenever the stored corrections change.
func (s *Store) SorterGeneration() (string, error) {
	return metaString(s.db, metaSorterGeneration)
}

// SorterCorrectionsStrict returns at most limit of the newest corrections
// embedded by model, newest first. A read failure is returned, never an empty
// result: training on nothing would silently forget everything the user taught.
func (s *Store) SorterCorrectionsStrict(model string, limit int) ([]SorterExample, error) {
	rows, err := s.db.Query(`SELECT label, vec FROM sorter_corrections WHERE model = ?
		ORDER BY at_unix DESC, rowid DESC LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SorterExample
	for rows.Next() {
		var (
			label string
			raw   []byte
		)
		if err := rows.Scan(&label, &raw); err != nil {
			return nil, err
		}
		vec, err := decodeVec(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, SorterExample{Label: label, Vec: vec})
	}
	return out, rows.Err()
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("sorter vector has %d bytes, not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, nil
}
