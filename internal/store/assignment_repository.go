package store

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/rhobuild/runpool/internal/store/sqlitedb"

	"github.com/rhobuild/runpool/internal/assignment"
)

var (
	// ErrContractDrift means the provider redelivered a natural key with
	// different content. The delivery is neither overwritten nor
	// acknowledged: drift is a broken upstream assumption, and the
	// binding stops until a person looks.
	ErrContractDrift = errors.New("delivery fingerprint differs under the same natural key")
	// ErrOpenAttemptExists means a workload already has an unresolved
	// attempt, so a new one may not open: two live attempts for one
	// workload is how a job runs twice.
	ErrOpenAttemptExists = errors.New("an open attempt already exists for this workload")
	// ErrConflict means a compare-and-swap matched no row: the row moved
	// since the caller observed it. Re-read and re-decide; the store
	// never resolves a conflict by forcing the write.
	ErrConflict = errors.New("row state moved since it was observed")
	// ErrNotFound mirrors sql.ErrNoRows as a domain error.
	ErrNotFound = errors.New("not found")
)

// WorkloadRow is the assignment content a delivery carries for one
// workload, in the domain's neutral terms.
type WorkloadRow struct {
	SourceWorkloadKey assignment.SourceWorkloadKey
	TenantKey         assignment.TenantKey
	ProjectKey        assignment.ProjectKey
}

// RecordDelivery persists one delivery and its attempts idempotently.
//
//   - First arrival inserts the delivery and one ready attempt per
//     workload.
//   - An exact redelivery (same natural key, same fingerprint) inserts
//     nothing and reports the existing delivery: at-least-once transport
//     must not double work.
//   - The same natural key with a different fingerprint returns
//     ErrContractDrift and writes nothing.
//   - A workload that already holds an open attempt under a *different*
//     delivery returns ErrOpenAttemptExists: the caller supersedes or
//     resolves the predecessor first, in the same transaction, and
//     retries.
func (t *Tx) RecordDelivery(bindingID assignment.BindingID, sourceDeliveryKey assignment.DeliveryKey,
	assigned []assignment.WorkloadAssignment, workloads []WorkloadRow) (assignment.DeliveryID, error) {
	delivery, err := t.q.GetDeliveryByKey(t.ctx, sqlitedb.GetDeliveryByKeyParams{
		BindingID: int64(bindingID), SourceDeliveryKey: string(sourceDeliveryKey),
	})
	switch {
	case err == nil:
		format := assignment.DeliveryFingerprintFormat(delivery.PayloadFingerprintFormat)
		expected, ok := assignment.DeliveryFingerprintForFormat(assigned, format)
		if !ok {
			return 0, fmt.Errorf("delivery %s of binding %d uses unsupported fingerprint format %q",
				sourceDeliveryKey, bindingID, format)
		}
		if !bytes.Equal(delivery.PayloadSha256, expected[:]) {
			return 0, fmt.Errorf("%w: delivery %s of binding %d",
				ErrContractDrift, sourceDeliveryKey, bindingID)
		}
		// Exact redelivery. The attempts are ensured below rather than
		// assumed: a failure between inserting the delivery and its
		// attempts — a crash, or an open-attempt conflict the caller is
		// resolving inside this very transaction — would otherwise leave
		// a delivery whose work no query can ever find.
	case errors.Is(err, sql.ErrNoRows):
		format, fingerprint := assignment.CurrentDeliveryFingerprint(assigned)
		delivery, err = t.q.InsertDelivery(t.ctx, sqlitedb.InsertDeliveryParams{
			BindingID:                int64(bindingID),
			SourceDeliveryKey:        string(sourceDeliveryKey),
			PayloadSha256:            fingerprint[:],
			PayloadFingerprintFormat: string(format),
		})
		if err != nil {
			return 0, err
		}
	default:
		return 0, err
	}

	for _, w := range workloads {
		if w.SourceWorkloadKey == "" {
			return assignment.DeliveryID(delivery.ID), fmt.Errorf("workload with no key in delivery %s cannot be made durable", sourceDeliveryKey)
		}
		if err := t.insertAttempt(delivery, w); err != nil {
			// The id travels with the error on purpose. The delivery row
			// exists in this transaction by now, and a caller resolving
			// an open-attempt conflict needs to know which delivery is
			// asking - without it, the only way to resolve the conflict
			// is to supersede blindly, including the attempts this very
			// delivery just inserted.
			return assignment.DeliveryID(delivery.ID), err
		}
	}
	return assignment.DeliveryID(delivery.ID), nil
}

func (t *Tx) insertAttempt(delivery sqlitedb.BrokerDelivery, w WorkloadRow) error {
	id, err := newAttemptID()
	if err != nil {
		return err
	}
	affected, err := t.q.InsertAttempt(t.ctx, sqlitedb.InsertAttemptParams{
		ID:                id,
		DeliveryID:        delivery.ID,
		BindingID:         delivery.BindingID,
		SourceWorkloadKey: string(w.SourceWorkloadKey),
		TenantKey:         string(w.TenantKey),
		ProjectKey:        string(w.ProjectKey),
	})
	if err != nil {
		// The partial unique index rejects a second open attempt for the
		// workload. That is a real conflict with an attempt under a
		// different delivery — the caller resolves the predecessor first.
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: workload %s of binding %d",
				ErrOpenAttemptExists, w.SourceWorkloadKey, delivery.BindingID)
		}
		return err
	}
	if affected == 0 {
		// Same delivery, same workload: already inserted by a previous
		// arrival of this delivery. Idempotent by design.
		return nil
	}
	_, err = t.q.InsertAttemptEvent(t.ctx, sqlitedb.InsertAttemptEventParams{
		AttemptID:      id,
		IdempotencyKey: "attempt_created",
		Kind:           string(EventAttemptCreated),
		DetailJSON:     "{}",
	})
	return err
}

// SupersedeOpenAttempt resolves the open predecessor of a workload so a
// new delivery's attempt can be recorded — in the caller's transaction,
// so old-to-superseded and new-to-ready commit together or not at all.
// Only an attempt that provably consumed nothing may be superseded;
// anything further along is settled or reviewed through its own path,
// and the redelivery waits for that to happen.
func (t *Tx) SupersedeOpenAttempt(bindingID assignment.BindingID, sourceWorkloadKey assignment.SourceWorkloadKey,
	resolution assignment.Resolution, exceptDelivery assignment.DeliveryID) error {
	open, err := t.q.GetOpenAttemptByWorkload(t.ctx, sqlitedb.GetOpenAttemptByWorkloadParams{
		BindingID: int64(bindingID), SourceWorkloadKey: string(sourceWorkloadKey),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// A delivery never supersedes its own attempts. RecordDelivery
	// inserts the workloads it reaches before the conflicting one, so a
	// caller resolving that conflict by looping over the delivery's
	// workloads finds those first: superseding one and retrying leaves
	// the retry's ON CONFLICT DO NOTHING reporting it as already
	// inserted, and the workload stays superseded, unserved, with its
	// message acknowledged. Nothing afterwards notices, because the
	// delivery did land.
	if assignment.DeliveryID(open.DeliveryID) == exceptDelivery {
		return ErrNotFound
	}
	affected, err := t.q.SupersedeAttempt(t.ctx, sqlitedb.SupersedeAttemptParams{
		Resolution: nullVocabulary(resolution), AttemptID: open.ID,
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s is %s and cannot be superseded",
			ErrConflict, open.ID, open.State)
	}
	_, err = t.q.InsertAttemptEvent(t.ctx, sqlitedb.InsertAttemptEventParams{
		AttemptID:      open.ID,
		IdempotencyKey: "attempt_superseded",
		Kind:           string(EventAttemptSuperseded),
		DetailJSON:     "{}",
	})
	return err
}

// isUniqueViolation reports only SQLite's extended UNIQUE result. Other
// constraints are programming or data errors and must not be translated into
// an open-attempt conflict merely because their message happens to contain a
// familiar phrase.
func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

func newAttemptID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "att-" + hex.EncodeToString(buf), nil
}
