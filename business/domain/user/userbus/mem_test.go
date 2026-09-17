package userbus_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// memStore is a userbus.Storer in a map, so the rules can be tested without a
// database.
//
// The mutex is not decoration. Three of the Storer methods are contracted to
// be a single atomic claim -- UseToken, UseBackupCode and ClaimBootstrap --
// and a test double that claims twice would let a real bug pass. Everything
// here holds the lock for the whole operation, which is the strongest version
// of that contract and therefore the one a racing test should be checked
// against.
type memStore struct {
	mu sync.Mutex

	users     map[types.ID]userbus.User
	tokens    map[types.ID]userbus.Token
	sessions  map[types.ID]userbus.Session
	codes     map[types.ID]userbus.BackupCode
	bootstrap bool

	// Counters, so a test can assert that a lookup happened at all -- the
	// enumeration-safety tests care that an unknown address does no more work
	// than a known one.
	tokensCreated int
}

func newMemStore() *memStore {
	return &memStore{
		users:    map[types.ID]userbus.User{},
		tokens:   map[types.ID]userbus.Token{},
		sessions: map[types.ID]userbus.Session{},
		codes:    map[types.ID]userbus.BackupCode{},
	}
}

func (m *memStore) CreateUser(_ context.Context, u userbus.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.users[u.ID] = u

	return nil
}

func (m *memStore) UpdateUser(_ context.Context, u userbus.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.users[u.ID]; !ok {
		return userbus.ErrNotFound
	}
	m.users[u.ID] = u

	return nil
}

func (m *memStore) UserByID(_ context.Context, id types.ID) (userbus.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	u, ok := m.users[id]
	if !ok {
		return userbus.User{}, userbus.ErrNotFound
	}

	return u, nil
}

func (m *memStore) UserByEmail(_ context.Context, email types.Email) (userbus.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, u := range m.users {
		if u.Email == email {
			return u, nil
		}
	}

	return userbus.User{}, userbus.ErrNotFound
}

func (m *memStore) Users(_ context.Context) ([]userbus.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := slices.Collect(maps.Values(m.users))
	slices.SortFunc(out, func(a, b userbus.User) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	return out, nil
}

func (m *memStore) CreateToken(_ context.Context, t userbus.Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tokens[t.ID] = t
	m.tokensCreated++

	return nil
}

func (m *memStore) TokenByID(_ context.Context, id types.ID) (userbus.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tokens[id]
	if !ok {
		return userbus.Token{}, userbus.ErrNotFound
	}

	return t, nil
}

// UseToken is the atomic claim: it succeeds for an unused row and fails for
// every subsequent caller, which is what a real UPDATE ... WHERE used_at IS
// NULL does.
func (m *memStore) UseToken(_ context.Context, id types.ID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tokens[id]
	if !ok || !t.UsedAt.IsZero() {
		return false, nil
	}

	t.UsedAt = at
	m.tokens[id] = t

	return true, nil
}

func (m *memStore) ReplaceBackupCodes(_ context.Context, userID types.ID, codes []userbus.BackupCode) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, c := range m.codes {
		if c.UserID == userID {
			delete(m.codes, id)
		}
	}

	for _, c := range codes {
		m.codes[c.ID] = c
	}

	return nil
}

func (m *memStore) BackupCodes(_ context.Context, userID types.ID) ([]userbus.BackupCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []userbus.BackupCode
	for _, c := range m.codes {
		if c.UserID == userID {
			out = append(out, c)
		}
	}

	// Sorted by id, so the order a test sees does not depend on Go's map
	// iteration. The domain checks every code regardless of order, which is
	// itself a property worth not accidentally relying on.
	slices.SortFunc(out, func(a, b userbus.BackupCode) int {
		return strings.Compare(a.ID.String(), b.ID.String())
	})

	return out, nil
}

func (m *memStore) UseBackupCode(_ context.Context, id types.ID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.codes[id]
	if !ok || !c.UsedAt.IsZero() {
		return false, nil
	}

	c.UsedAt = at
	m.codes[id] = c

	return true, nil
}

func (m *memStore) CreateSession(_ context.Context, s userbus.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sessions[s.ID] = s

	return nil
}

func (m *memStore) SessionByID(_ context.Context, id types.ID) (userbus.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return userbus.Session{}, userbus.ErrNotFound
	}

	return s, nil
}

func (m *memStore) TouchSession(_ context.Context, id types.ID, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return userbus.ErrNotFound
	}

	s.LastSeenAt = at
	m.sessions[id] = s

	return nil
}

func (m *memStore) DeleteSession(_ context.Context, id types.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.sessions, id)

	return nil
}

func (m *memStore) DeleteUserSessions(_ context.Context, userID types.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, s := range m.sessions {
		if s.UserID == userID {
			delete(m.sessions, id)
		}
	}

	return nil
}

func (m *memStore) ClaimBootstrap(_ context.Context, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.bootstrap {
		return false, nil
	}
	m.bootstrap = true

	return true, nil
}

func (m *memStore) PruneExpired(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, t := range m.tokens {
		if t.ExpiresAt.Before(before) {
			delete(m.tokens, id)
		}
	}
	for id, s := range m.sessions {
		if s.ExpiresAt.Before(before) {
			delete(m.sessions, id)
		}
	}

	return nil
}

// storedSecrets returns every byte this store holds that a secret could have
// leaked into, so a test can assert none of them contains one.
func (m *memStore) storedSecrets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []string

	for _, t := range m.tokens {
		out = append(out, string(t.Hash))
	}
	for _, s := range m.sessions {
		out = append(out, string(s.Hash))
	}
	for _, c := range m.codes {
		out = append(out, string(c.Hash))
	}

	return out
}
