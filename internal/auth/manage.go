package auth

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/trevorswanson/composecd/internal/store"
)

// MinPasswordLength is the shortest password a local user may have.
const MinPasswordLength = 10

var nameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{0,62}$`)

// InvalidError is a request that can't be accepted as given (a bad name or a
// short password), as opposed to a failure.
type InvalidError struct{ msg string }

func (e *InvalidError) Error() string { return e.msg }

func invalid(format string, args ...interface{}) error {
	return &InvalidError{msg: fmt.Sprintf(format, args...)}
}

func validName(kind, name string) error {
	if !nameRegexp.MatchString(name) {
		return invalid("invalid %s name %q: use lowercase letters, digits, '.', '_', '@' or '-', at most 63 characters", kind, name)
	}
	return nil
}

// bcryptCost is the work factor for new password hashes (tests lower it).
var bcryptCost = bcrypt.DefaultCost

// dummyHash makes failed sign-ins for unknown users as slow as for known ones.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("composecd-dummy-password"), bcrypt.DefaultCost)

func hashPassword(pw string) (string, error) {
	if len(pw) < MinPasswordLength {
		return "", invalid("the password must be at least %d characters", MinPasswordLength)
	}
	if len(pw) > 72 {
		return "", invalid("the password must be at most 72 bytes")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

func checkPassword(hash []byte, pw string) error {
	return bcrypt.CompareHashAndPassword(hash, []byte(pw))
}

// AddUser creates a local user.
func (a *Authenticator) AddUser(name string, role Role, password string) error {
	if err := validName("user", name); err != nil {
		return err
	}
	if name == "admin" {
		return invalid(`"admin" is reserved for the bootstrap admin token`)
	}
	h, err := hashPassword(password)
	if err != nil {
		return err
	}
	return a.Store.AddUser(&store.User{Name: name, Role: string(role), PasswordHash: h})
}

// SetPassword changes a local user's password, signing them out everywhere.
func (a *Authenticator) SetPassword(name, password string) error {
	h, err := hashPassword(password)
	if err != nil {
		return err
	}
	return a.Store.SetPassword(name, h)
}

// Token prefix and shape: ccd_<id>_<secret>.
const tokenPrefix = "ccd_"

func parseToken(tok string) (id, secret string, ok bool) {
	rest, ok := strings.CutPrefix(tok, tokenPrefix)
	if !ok {
		return "", "", false
	}
	id, secret, ok = strings.Cut(rest, "_")
	return id, secret, ok && len(id) == 16 && len(secret) == 64
}

// CreateToken makes an API token and returns it; it can't be shown again.
func (a *Authenticator) CreateToken(name string, role Role, by string) (string, error) {
	if err := validName("token", name); err != nil {
		return "", err
	}
	id, err := random(8)
	if err != nil {
		return "", err
	}
	secret, err := random(32)
	if err != nil {
		return "", err
	}
	if err := a.Store.AddToken(&store.Token{ID: id, Name: name, Role: string(role), SecretHash: hash(secret), CreatedBy: by}); err != nil {
		return "", err
	}
	return tokenPrefix + id + "_" + secret, nil
}

// RevokeToken deletes a token and ends the sessions signed in with it.
func (a *Authenticator) RevokeToken(name string) error {
	if err := a.Store.DeleteToken(name); err != nil {
		return err
	}
	return a.Store.DeleteSessions(name, ViaToken)
}

// limiter locks out a name and address after repeated failed sign-ins: five
// failures lock for a minute, doubling with each further failure up to 15
// minutes.
type limiter struct {
	mu   sync.Mutex
	keys map[string]*attempts
}

type attempts struct {
	failures int
	until    time.Time
	last     time.Time
}

const (
	freeFailures = 5
	lockBase     = time.Minute
	lockMax      = 15 * time.Minute
)

func (l *limiter) allowed(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.keys[key]
	return a == nil || !now.Before(a.until)
}

func (l *limiter) failed(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.keys == nil {
		l.keys = map[string]*attempts{}
	}
	// Forget old entries so the map can't grow without bound.
	for k, a := range l.keys {
		if now.Sub(a.last) > 24*time.Hour {
			delete(l.keys, k)
		}
	}
	a := l.keys[key]
	if a == nil {
		a = &attempts{}
		l.keys[key] = a
	}
	a.failures++
	a.last = now
	if a.failures >= freeFailures {
		d := lockBase << (a.failures - freeFailures)
		if d > lockMax || d <= 0 {
			d = lockMax
		}
		a.until = now.Add(d)
	}
}

func (l *limiter) succeeded(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}
