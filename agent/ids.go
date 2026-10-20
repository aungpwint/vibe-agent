package agent

import (
	"bytes"
	crand "crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

type ID string

var ErrInvalidID = errors.New("not a valid identifier")

var (
	entropyMu sync.Mutex
	lastMS    uint64
	lastID    ulid.ULID
	haveLast  bool
)

// NewID returns a ULID whose first six bytes are the timestamp. Two ids minted
// inside the same millisecond still sort in creation order: the entropy is
// copied from the previous id and incremented, so the local database never
// receives an id that sorts before one already written.
func NewID(now time.Time) ID {
	ms := ulid.Timestamp(now)

	entropyMu.Lock()
	defer entropyMu.Unlock()

	var id ulid.ULID
	if err := id.SetTime(ms); err != nil {
		panic("agent: cannot encode " + now.String() + " as an identifier time: " + err.Error())
	}
	if _, err := io.ReadFull(crand.Reader, id[6:]); err != nil {
		panic("agent: cannot read entropy for an identifier: " + err.Error())
	}
	if haveLast && lastMS == ms && bytes.Compare(id[6:], lastID[6:]) <= 0 {
		copy(id[6:], lastID[6:])
		incrementEntropy(id[6:])
	}
	lastMS, lastID, haveLast = ms, id, true
	return ID(id.String())
}

func incrementEntropy(b []byte) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

func ParseID(raw string) (ID, error) {
	parsed, err := ulid.ParseStrict(strings.TrimSpace(raw))
	if err != nil {
		return "", ErrInvalidID
	}
	return ID(parsed.String()), nil
}

func (id ID) IsZero() bool { return id == "" }
