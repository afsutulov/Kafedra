package secure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WithSnapshot captures open descriptors and fixed lengths under the audit lock.
// Verification and potentially slow compression/output run without that lock.
// Log files are append-only: new events cannot enter the captured prefix.
func (a *Audit) WithSnapshot(write func(name string, r io.Reader) error) error {
	type part struct {
		f    *os.File
		name string
		size int64
	}
	var parts []part
	defer func() {
		for _, p := range parts {
			p.f.Close()
		}
	}()
	var head, checkpoint auditHead
	err := func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.failed != nil {
			return a.failed
		}
		head = auditHead{Seq: a.seq, Hash: a.prev, First: a.first, Anchor: a.anchor}
		var err error
		checkpoint, err = readAuditHead(a.dir)
		if err != nil {
			return err
		}
		if checkpoint.Seq > head.Seq || max(checkpoint.First, 1) != max(head.First, 1) || checkpoint.Anchor != head.Anchor {
			return errors.New("контрольная точка аудита изменилась после запуска")
		}
		for _, name := range AuditFiles(a.dir) {
			f, err := os.Open(name)
			if err != nil {
				return err
			}
			st, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			parts = append(parts, part{f: f, name: filepath.Base(name), size: st.Size()})
		}
		return nil
	}()
	if err != nil {
		return err
	}
	seq, hash := max(head.First, 1)-1, head.Anchor
	for _, p := range parts {
		err := scanAuditReader(io.NewSectionReader(p.f, 0, p.size), p.name, func(r AuditRecord) error {
			if r.Seq != seq+1 || r.Prev != hash || r.Hash != r.sum() || (r.Seq == checkpoint.Seq && r.Hash != checkpoint.Hash) {
				return fmt.Errorf("нарушена целостность снимка аудита: %s, запись %d", p.name, r.Seq)
			}
			seq, hash = r.Seq, r.Hash
			return nil
		})
		if err != nil {
			return err
		}
	}
	if seq != head.Seq || hash != head.Hash {
		return errors.New("журнал аудита изменён после запуска")
	}
	for _, p := range parts {
		if err := write(p.name, io.NewSectionReader(p.f, 0, p.size)); err != nil {
			return err
		}
	}
	if head.Seq > 0 {
		data, err := json.Marshal(head)
		if err != nil {
			return err
		}
		return write(AuditHeadFile, bytes.NewReader(data))
	}
	return nil
}
