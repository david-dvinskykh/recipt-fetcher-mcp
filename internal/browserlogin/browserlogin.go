// Package browserlogin runs the interactive, multi-step store logins that back
// the button-auth flow (see docs/button-auth.md).
//
// A login is modelled as a small state machine: a Driver is asked for the next
// Form to show the user, is given the user's Values, and eventually reports Done
// with the credentials to persist. Drivers that need a live headless browser
// across steps (Lidl's phone → password → SMS) run as a goroutine holding the
// browser context; the Manager brokers Values in and Forms out over channels so
// an HTTP handler can drive the flow with ordinary request/response round trips.
package browserlogin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Field is one input on a login form.
type Field struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type"` // "text", "password", "tel"
	Placeholder string `json:"placeholder,omitempty"`
	Required    bool   `json:"required"`
}

// Form is a step the user has to fill in.
type Form struct {
	Title  string  `json:"title"`
	Note   string  `json:"note,omitempty"`
	Fields []Field `json:"fields"`
}

// Outcome is what a driver produces after a step: either another Form to show,
// or a completed login with the credentials to store.
type Outcome struct {
	// Form is the next step to show; nil when the login is complete.
	Form *Form
	// Done is true when the login succeeded.
	Done bool
	// Stored are the fields to persist for the provider (e.g. refresh_token).
	Stored map[string]string
	// Message is a human summary shown on completion.
	Message string
}

// Prompt asks the user for a form and waits for the values. A Driver calls it to
// suspend until the next HTTP step arrives.
type Prompt func(Form) (values map[string]string, err error)

// Driver runs one provider's interactive login. Run should call ask() for each
// form it needs, and return the credentials to store. Returning an error aborts
// the session. Run must respect ctx cancellation (session timeout / cancel).
type Driver interface {
	// Provider is the store id the captured credentials belong to.
	Provider() string
	// Run drives the login, prompting for input via ask, and returns the fields
	// to persist for the provider on success.
	Run(ctx context.Context, ask Prompt) (stored map[string]string, message string, err error)
}

// ErrNoSession is returned when a session id is unknown or has expired.
var ErrNoSession = errors.New("login session not found or expired")

// ErrCancelled is returned to a driver when its session is cancelled.
var ErrCancelled = errors.New("login session cancelled")

// sessionTTL bounds how long a half-finished login may sit waiting for the next
// step before its browser context is torn down.
const sessionTTL = 5 * time.Minute

// Manager owns the live login sessions.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*session
}

// NewManager builds an empty manager.
func NewManager() *Manager {
	return &Manager{sessions: map[string]*session{}}
}

type session struct {
	id       string
	provider string

	// forms carries a Form from the driver goroutine to the HTTP side; values
	// carries the user's answers back. done carries the terminal outcome.
	forms  chan Form
	values chan map[string]string
	done   chan Outcome
	fail   chan error

	cancel  context.CancelFunc
	expires time.Time
}

// StepResult is what an HTTP handler returns to the page after start/step.
type StepResult struct {
	Session  string            `json:"session,omitempty"`
	Provider string            `json:"provider"`
	Step     *Form             `json:"step,omitempty"`
	Done     bool              `json:"done"`
	Stored   map[string]string `json:"-"` // never serialized; persisted server-side
	Message  string            `json:"message,omitempty"`
}

// Start begins a login with the given driver and returns the first step.
func (m *Manager) Start(parent context.Context, driver Driver) (StepResult, error) {
	m.sweep()

	id, err := newID()
	if err != nil {
		return StepResult{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		id:       id,
		provider: driver.Provider(),
		forms:    make(chan Form),
		values:   make(chan map[string]string),
		done:     make(chan Outcome, 1),
		fail:     make(chan error, 1),
		cancel:   cancel,
		expires:  time.Now().Add(sessionTTL),
	}

	ask := func(form Form) (map[string]string, error) {
		select {
		case s.forms <- form:
		case <-ctx.Done():
			return nil, ErrCancelled
		}
		select {
		case v := <-s.values:
			return v, nil
		case <-ctx.Done():
			return nil, ErrCancelled
		}
	}

	go func() {
		stored, message, err := driver.Run(ctx, ask)
		if err != nil {
			s.fail <- err
			return
		}
		s.done <- Outcome{Done: true, Stored: stored, Message: message}
	}()

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	res, err := s.await(parent)
	m.dropIfSettled(id, res, err)
	return res, err
}

// Step submits the user's values for the current form and returns the next.
func (m *Manager) Step(parent context.Context, id string, values map[string]string) (StepResult, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return StepResult{}, ErrNoSession
	}

	select {
	case s.values <- values:
	case <-parent.Done():
		return StepResult{}, parent.Err()
	case out := <-s.done:
		m.drop(id)
		return terminal(s, out), nil
	case err := <-s.fail:
		m.drop(id)
		return StepResult{}, err
	}
	res, err := s.await(parent)
	m.dropIfSettled(id, res, err)
	return res, err
}

// dropIfSettled tears a session down once it has completed or errored, so a
// later step against the same id fails fast with ErrNoSession.
func (m *Manager) dropIfSettled(id string, res StepResult, err error) {
	if err != nil || res.Done {
		m.drop(id)
	}
}

// Cancel tears a session down.
func (m *Manager) Cancel(id string) {
	m.drop(id)
}

// await waits for the driver to emit the next form or a terminal outcome.
func (s *session) await(parent context.Context) (StepResult, error) {
	select {
	case form := <-s.forms:
		s.expires = time.Now().Add(sessionTTL)
		return StepResult{Session: s.id, Provider: s.provider, Step: &form}, nil
	case out := <-s.done:
		return terminal(s, out), nil
	case err := <-s.fail:
		return StepResult{}, err
	case <-parent.Done():
		return StepResult{}, parent.Err()
	case <-time.After(sessionTTL):
		return StepResult{}, fmt.Errorf("login timed out")
	}
}

func terminal(s *session, out Outcome) StepResult {
	return StepResult{
		Session:  s.id,
		Provider: s.provider,
		Done:     true,
		Stored:   out.Stored,
		Message:  out.Message,
	}
}

func (m *Manager) drop(id string) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if ok {
		s.cancel()
	}
}

// sweep tears down sessions whose TTL has passed.
func (m *Manager) sweep() {
	now := time.Now()
	m.mu.Lock()
	var expired []*session
	for id, s := range m.sessions {
		if now.After(s.expires) {
			expired = append(expired, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range expired {
		s.cancel()
	}
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
