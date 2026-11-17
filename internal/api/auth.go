package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/jxburros/buttercams/internal/config"
)

// Roles (spec 11.1).
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// Login protection (spec 11.1).
const (
	MaxFailedLogins    = 5
	LockoutDuration    = 15 * time.Minute
	SessionIdleTimeout = 12 * time.Hour
	MinPasswordLength  = 8
	SessionCookie      = "buttercams_session"
	CSRFHeader         = "X-CSRF-Token"
)

// UsersFile is the account file inside the data root. Accounts live outside
// the database on purpose: the database can be deleted and rebuilt from the
// recordings, and that must not lock anyone out.
const UsersFile = "users.json"

// argon2id parameters (OWASP's recommended minimum configuration).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// hashGate allows one password hash at a time, so a burst of login attempts
// cannot take CPU or memory away from the rest of the service.
var hashGate = make(chan struct{}, 1)

// HashPassword returns an argon2id hash in the standard encoded form.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hashGate <- struct{}{}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-hashGate
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against an encoded argon2id hash.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var ver int
	var mem, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &ver); err != nil || ver != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false
	}
	if mem > 1<<20 || t > 16 || p == 0 || p > 16 { // refuse absurd parameters from a tampered file
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	hashGate <- struct{}{}
	got := argon2.IDKey([]byte(password), salt, t, mem, p, uint32(len(want)))
	<-hashGate
	return subtle.ConstantTimeCompare(got, want) == 1
}

// User is one account.
type User struct {
	Username     string    `json:"username"`
	Role         string    `json:"role"`
	PasswordHash string    `json:"passwordHash"`
	CreatedAt    time.Time `json:"createdAt"`
	// Cameras limits a Viewer to these camera ids; nil means every camera
	// (spec 16.6). It is ignored for Admins, who always see everything.
	Cameras *[]string `json:"cameras"`
	// Views are the user's own saved views of the Live page.
	Views []config.View `json:"views,omitempty"`
}

// Scope is the set of cameras a signed-in user may see.
type Scope struct {
	all bool
	set map[string]bool
}

// AllCameras is the scope of Admins and of Viewers without a limit.
var AllCameras = Scope{all: true}

// All reports whether the scope has no limit.
func (sc Scope) All() bool { return sc.all }

// Allows reports whether the camera is inside the scope.
func (sc Scope) Allows(cameraID string) bool { return sc.all || sc.set[cameraID] }

// IDs returns the cameras of a limited scope, sorted.
func (sc Scope) IDs() []string {
	out := make([]string, 0, len(sc.set))
	for id := range sc.set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// scope returns what the user may see.
func (u User) scope() Scope {
	if u.Role == RoleAdmin || u.Cameras == nil {
		return AllCameras
	}
	sc := Scope{set: make(map[string]bool, len(*u.Cameras))}
	for _, id := range *u.Cameras {
		sc.set[id] = true
	}
	return sc
}

type usersDoc struct {
	Users []User `json:"users"`
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// FieldErr builds a validation error for one field.
func FieldErr(field, msg string) error {
	return &config.ValidationError{Fields: []config.FieldError{{Field: field, Message: msg}}}
}

// ValidateNewUser checks a username, password and role.
func ValidateNewUser(username, password, role string) error {
	ve := &config.ValidationError{}
	if !usernameRe.MatchString(username) {
		ve.Fields = append(ve.Fields, config.FieldError{Field: "username",
			Message: "The user name may contain letters, digits, dots, hyphens, underscores and @, up to 64 characters."})
	}
	if len(password) < MinPasswordLength {
		ve.Fields = append(ve.Fields, config.FieldError{Field: "password",
			Message: fmt.Sprintf("The password needs at least %d characters.", MinPasswordLength)})
	}
	if role != RoleAdmin && role != RoleViewer {
		ve.Fields = append(ve.Fields, config.FieldError{Field: "role", Message: `The role must be "admin" or "viewer".`})
	}
	if len(ve.Fields) > 0 {
		return ve
	}
	return nil
}

// UserStore keeps the accounts in <dataRoot>/users.json.
type UserStore struct {
	mu    sync.Mutex
	path  string
	users []User

	failures map[string]*loginFailures
	now      func() time.Time
}

type loginFailures struct {
	count       int
	lockedUntil time.Time
	last        time.Time
}

// OpenUserStore loads the accounts. A missing file is an empty store.
func OpenUserStore(dataRoot string) (*UserStore, error) {
	s := &UserStore{path: filepath.Join(dataRoot, UsersFile), failures: map[string]*loginFailures{}, now: time.Now}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var doc usersDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", UsersFile, err)
	}
	s.users = doc.Users
	return s, nil
}

func (s *UserStore) saveLocked() error {
	data, err := json.MarshalIndent(usersDoc{Users: s.users}, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(s.path, append(data, '\n'))
}

func (s *UserStore) findLocked(username string) int {
	for i, u := range s.users {
		if strings.EqualFold(u.Username, username) {
			return i
		}
	}
	return -1
}

// Count returns the number of accounts.
func (s *UserStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.users)
}

// List returns the accounts without their password hashes.
func (s *UserStore) List() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]User, len(s.users))
	for i, u := range s.users {
		u.PasswordHash = ""
		out[i] = u
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Username) < strings.ToLower(out[j].Username) })
	return out
}

// Get returns one account without its password hash.
func (s *UserStore) Get(username string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.findLocked(username)
	if i < 0 {
		return User{}, false
	}
	u := s.users[i]
	u.PasswordHash = ""
	return u, true
}

// ScopeOf returns the cameras a user may see. An unknown user sees none.
func (s *UserStore) ScopeOf(username string) Scope {
	u, ok := s.Get(username)
	if !ok {
		return Scope{}
	}
	return u.scope()
}

// SetCameras limits a user to a list of cameras; nil removes the limit.
func (s *UserStore) SetCameras(username string, cameras *[]string) (User, error) {
	cameras, err := CleanCameraLimit(cameras)
	if err != nil {
		return User{}, err
	}
	return s.change(username, func(u *User) { u.Cameras = cameras })
}

// CleanCameraLimit checks and tidies a camera limit (nil = every camera):
// blanks and duplicates go, and ids and the list are bounded.
func CleanCameraLimit(cameras *[]string) (*[]string, error) {
	if cameras == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(*cameras))
	for _, id := range *cameras {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if len(id) > 64 {
			return nil, FieldErr("cameras", "A camera id is too long.")
		}
		seen[id] = true
		clean = append(clean, id)
	}
	if len(clean) > 1000 {
		return nil, FieldErr("cameras", "Too many cameras.")
	}
	return &clean, nil
}

// SetViews replaces a user's own saved views.
func (s *UserStore) SetViews(username string, views []config.View) (User, error) {
	views = config.NormalizeViews(views, nil)
	if fields := config.ValidateViews(views, "views"); len(fields) > 0 {
		return User{}, &config.ValidationError{Fields: fields}
	}
	return s.change(username, func(u *User) { u.Views = views })
}

func (s *UserStore) change(username string, fn func(*User)) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.findLocked(username)
	if i < 0 {
		return User{}, ErrNoSuchUser
	}
	prev := s.users[i]
	fn(&s.users[i])
	if err := s.saveLocked(); err != nil {
		s.users[i] = prev
		return User{}, err
	}
	u := s.users[i]
	u.PasswordHash = ""
	return u, nil
}

// Create adds an account.
func (s *UserStore) Create(username, password, role string) (User, error) {
	if err := ValidateNewUser(username, password, role); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findLocked(username) >= 0 {
		return User{}, FieldErr("username", "A user with this name already exists.")
	}
	u := User{Username: username, Role: role, PasswordHash: hash, CreatedAt: s.now().UTC().Truncate(time.Second)}
	s.users = append(s.users, u)
	if err := s.saveLocked(); err != nil {
		s.users = s.users[:len(s.users)-1]
		return User{}, err
	}
	u.PasswordHash = ""
	return u, nil
}

// ErrLastAdmin is returned when a change would leave no administrator.
var ErrLastAdmin = errors.New("the last administrator cannot be removed or demoted")

// ErrNoSuchUser is returned for an unknown user name.
var ErrNoSuchUser = errors.New("no such user")

func (s *UserStore) adminsLocked() int {
	n := 0
	for _, u := range s.users {
		if u.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// Update changes an account's password and/or role. Empty values keep the
// current setting.
func (s *UserStore) Update(username, password, role string) (User, error) {
	if password != "" && len(password) < MinPasswordLength {
		return User{}, FieldErr("password", fmt.Sprintf("The password needs at least %d characters.", MinPasswordLength))
	}
	if role != "" && role != RoleAdmin && role != RoleViewer {
		return User{}, FieldErr("role", `The role must be "admin" or "viewer".`)
	}
	hash := ""
	if password != "" {
		var err error
		if hash, err = HashPassword(password); err != nil {
			return User{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.findLocked(username)
	if i < 0 {
		return User{}, ErrNoSuchUser
	}
	prev := s.users[i]
	if role != "" && role != prev.Role {
		if prev.Role == RoleAdmin && s.adminsLocked() == 1 {
			return User{}, ErrLastAdmin
		}
		s.users[i].Role = role
	}
	if hash != "" {
		s.users[i].PasswordHash = hash
		delete(s.failures, strings.ToLower(username))
	}
	if err := s.saveLocked(); err != nil {
		s.users[i] = prev
		return User{}, err
	}
	u := s.users[i]
	u.PasswordHash = ""
	return u, nil
}

// Delete removes an account.
func (s *UserStore) Delete(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.findLocked(username)
	if i < 0 {
		return ErrNoSuchUser
	}
	if s.users[i].Role == RoleAdmin && s.adminsLocked() == 1 {
		return ErrLastAdmin
	}
	prev := append([]User(nil), s.users...)
	s.users = append(s.users[:i], s.users[i+1:]...)
	if err := s.saveLocked(); err != nil {
		s.users = prev
		return err
	}
	return nil
}

// ErrLocked is returned by Authenticate while an account is locked.
type ErrLocked struct{ Until time.Time }

func (e *ErrLocked) Error() string {
	mins := int(time.Until(e.Until).Minutes()) + 1
	return fmt.Sprintf("Too many failed sign-in attempts. Try again in %d minutes.", mins)
}

// ErrBadCredentials is returned for a wrong user name or password.
var ErrBadCredentials = errors.New("The user name or password is not correct.")

// dummyHash is verified for unknown user names so that a wrong user name
// takes as long as a wrong password.
var dummyHash, _ = HashPassword("buttercams-timing-equaliser")

// Locked reports whether an account is currently locked.
func (s *UserStore) Locked(username string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.failures[strings.ToLower(username)]
	return f != nil && s.now().Before(f.lockedUntil)
}

// Authenticate checks a login. Five failures in a row lock the account for
// fifteen minutes (spec 11.1).
func (s *UserStore) Authenticate(username, password string) (User, error) {
	key := strings.ToLower(username)
	s.mu.Lock()
	now := s.now()
	if f := s.failures[key]; f != nil && now.Before(f.lockedUntil) {
		until := f.lockedUntil
		s.mu.Unlock()
		return User{}, &ErrLocked{Until: until}
	}
	var u User
	found := false
	if i := s.findLocked(username); i >= 0 {
		u, found = s.users[i], true
	}
	s.mu.Unlock()

	ok := false
	if found {
		ok = VerifyPassword(u.PasswordHash, password)
	} else {
		VerifyPassword(dummyHash, password)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		delete(s.failures, key)
		u.PasswordHash = ""
		return u, nil
	}
	if len(s.failures) > 10000 { // bound the memory an attacker can make us keep
		for k, f := range s.failures {
			if now.After(f.lockedUntil) && now.Sub(f.last) > LockoutDuration {
				delete(s.failures, k)
			}
		}
	}
	f := s.failures[key]
	if f == nil {
		f = &loginFailures{}
		s.failures[key] = f
	}
	f.count++
	f.last = now
	if f.count >= MaxFailedLogins {
		f.lockedUntil = now.Add(LockoutDuration)
		f.count = 0
		return User{}, &ErrLocked{Until: f.lockedUntil}
	}
	return User{}, ErrBadCredentials
}

// ------------------------------------------------------------------ sessions

type session struct {
	token    string
	username string
	role     string
	csrf     string
	lastSeen time.Time
	issued   time.Time
}

type sessions struct {
	mu  sync.Mutex
	m   map[string]*session
	now func() time.Time
}

func newSessions() *sessions {
	return &sessions{m: map[string]*session{}, now: time.Now}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source is unavailable
	}
	return hex.EncodeToString(b)
}

func (s *sessions) create(u User) *session {
	now := s.now()
	sess := &session{token: randomToken(), username: u.Username, role: u.Role, csrf: randomToken(), lastSeen: now, issued: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m { // drop sessions that idled out
		if now.Sub(v.lastSeen) > SessionIdleTimeout {
			delete(s.m, k)
		}
	}
	s.m[sess.token] = sess
	return sess
}

// get returns a copy of the live session for a token and marks it used.
func (s *sessions) get(token string) (session, bool) {
	if token == "" {
		return session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[token]
	if sess == nil {
		return session{}, false
	}
	now := s.now()
	if now.Sub(sess.lastSeen) > SessionIdleTimeout {
		delete(s.m, token)
		return session{}, false
	}
	sess.lastSeen = now
	return *sess, true
}

// peek returns a copy of a live session without counting it as use.
func (s *sessions) peek(token string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[token]
	if sess == nil || s.now().Sub(sess.lastSeen) > SessionIdleTimeout {
		return session{}, false
	}
	return *sess, true
}

// alive reports whether a session still exists and has not idled out,
// without counting as use. Long-lived responses (the event stream, live
// video) check it so they end when the session ends.
func (s *sessions) alive(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[token]
	return sess != nil && s.now().Sub(sess.lastSeen) <= SessionIdleTimeout
}

func (s *sessions) reissued(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.m[token]; sess != nil {
		sess.issued = s.now()
	}
}

func (s *sessions) delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, token)
}

// dropUser ends every session of a user (deleted, demoted or new password).
func (s *sessions) dropUser(username string, except string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if strings.EqualFold(v.username, username) && k != except {
			delete(s.m, k)
		}
	}
}

func (s *sessions) setRole(username, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.m {
		if strings.EqualFold(v.username, username) {
			v.role = role
		}
	}
}
