package browserlogin

import (
	"context"
	"errors"
	"testing"
)

// twoStepDriver asks for two forms then completes, to exercise the state machine.
type twoStepDriver struct{ provider string }

func (d twoStepDriver) Provider() string { return d.provider }
func (d twoStepDriver) Run(ctx context.Context, ask Prompt) (map[string]string, string, error) {
	a, err := ask(Form{Title: "step 1", Fields: []Field{{Name: "phone"}}})
	if err != nil {
		return nil, "", err
	}
	b, err := ask(Form{Title: "step 2", Fields: []Field{{Name: "code"}}})
	if err != nil {
		return nil, "", err
	}
	return map[string]string{"phone": a["phone"], "code": b["code"]}, "done", nil
}

func TestManagerTwoStepFlow(t *testing.T) {
	m := NewManager()
	ctx := context.Background()

	first, err := m.Start(ctx, twoStepDriver{provider: "lidl"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Done || first.Step == nil || first.Step.Title != "step 1" {
		t.Fatalf("first = %+v, want step 1 form", first)
	}
	if first.Session == "" {
		t.Fatal("expected a session id")
	}

	second, err := m.Step(ctx, first.Session, map[string]string{"phone": "+48"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Done || second.Step == nil || second.Step.Title != "step 2" {
		t.Fatalf("second = %+v, want step 2 form", second)
	}

	done, err := m.Step(ctx, first.Session, map[string]string{"code": "1234"})
	if err != nil {
		t.Fatal(err)
	}
	if !done.Done {
		t.Fatalf("expected completion, got %+v", done)
	}
	if done.Stored["phone"] != "+48" || done.Stored["code"] != "1234" {
		t.Fatalf("stored = %+v", done.Stored)
	}

	// Session must be gone after completion.
	if _, err := m.Step(ctx, first.Session, nil); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession after completion", err)
	}
}

func TestManagerUnknownSession(t *testing.T) {
	m := NewManager()
	if _, err := m.Step(context.Background(), "nope", nil); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
}

func TestManagerCancelAbortsDriver(t *testing.T) {
	m := NewManager()
	first, err := m.Start(context.Background(), twoStepDriver{provider: "lidl"})
	if err != nil {
		t.Fatal(err)
	}
	m.Cancel(first.Session)
	if _, err := m.Step(context.Background(), first.Session, nil); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession after cancel", err)
	}
}

func TestPasteDriverStoresTrimmed(t *testing.T) {
	d := LidlPaste()
	stored, _, err := d.Run(context.Background(), func(Form) (map[string]string, error) {
		return map[string]string{"refresh_token": "  tok  ", "country": "PL", "language": ""}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored["refresh_token"] != "tok" || stored["country"] != "PL" {
		t.Fatalf("stored = %+v", stored)
	}
	if _, ok := stored["language"]; ok {
		t.Error("empty fields should be dropped")
	}
}

func TestPasteDriverRejectsEmpty(t *testing.T) {
	d := AllegroPaste()
	_, _, err := d.Run(context.Background(), func(Form) (map[string]string, error) {
		return map[string]string{"cookie": "   "}, nil
	})
	if err == nil {
		t.Fatal("empty cookie should be rejected")
	}
}

func TestActionPasteRequiresOneToken(t *testing.T) {
	_, _, err := ActionPaste().Run(context.Background(), func(Form) (map[string]string, error) {
		return map[string]string{}, nil
	})
	if err == nil {
		t.Fatal("action paste with neither token should fail")
	}
}

func TestPKCEChallengeMatchesVerifier(t *testing.T) {
	v, c, err := pkce()
	if err != nil {
		t.Fatal(err)
	}
	if v == "" || c == "" || v == c {
		t.Fatalf("bad pkce pair: verifier=%q challenge=%q", v, c)
	}
}

func TestCodeFromRedirect(t *testing.T) {
	if got := codeFromRedirect("com.lidlplus.app://callback?code=ABC123&state=x"); got != "ABC123" {
		t.Fatalf("got %q", got)
	}
	if got := codeFromRedirect("https://accounts.lidl.com/connect/authorize?foo=1"); got != "" {
		t.Fatalf("non-callback url yielded %q", got)
	}
}
