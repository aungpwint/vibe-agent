package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aungpwint/vibe-agent/store"
)

// Service is the whole write-and-read surface of the local store. Every method
// is safe for concurrent use: the pool serialises through PostgreSQL, and the
// only coordination the domain needs beyond SQL constraints is the sequence
// allocation inside allocateSequence.
type Service struct {
	pool *store.Pool
	now  func() time.Time
}

func New(pool *store.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Pool exposes the connection pool for the maintenance commands (migrate and
// doctor). The domain itself never leaks the driver upwards.
func (s *Service) Pool() *store.Pool { return s.pool }

func profileError(err error) error {
	if store.IsNoRows(err) {
		return ErrProfileNotFound
	}
	return unavailable(err)
}

func sessionError(err error) error {
	if store.IsNoRows(err) {
		return ErrSessionNotFound
	}
	return unavailable(err)
}

func decodeMetadata(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func decodeRuleset(raw []byte) (Ruleset, error) {
	if len(raw) == 0 {
		return Ruleset{}, nil
	}
	var out Ruleset
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = Ruleset{}
	}
	return out, nil
}

func decodeTools(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func decodeParts(raw []byte) ([]Part, error) {
	if len(raw) == 0 {
		return []Part{}, nil
	}
	var out []Part
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Part{}
	}
	return out, nil
}

const (
	defaultLimit = 20
	maxLimit     = 200
)

func normalizeLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultLimit
	case limit > maxLimit:
		return maxLimit
	default:
		return limit
	}
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type txFunc func(tx pgx.Tx) error

func (s *Service) inTx(ctx context.Context, fn txFunc) error {
	return s.pool.InTx(ctx, fn)
}
