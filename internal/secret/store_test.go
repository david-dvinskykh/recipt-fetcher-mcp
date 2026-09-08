package secret

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTripAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("lidl", map[string]string{"refresh_token": "secret-token", "country": "PL"}); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Field("lidl", "refresh_token"); got != "secret-token" {
		t.Fatalf("token after reopen = %q", got)
	}
	if got := reopened.FieldNames("lidl"); strings.Join(got, ",") != "country,refresh_token" {
		t.Fatalf("field names = %v", got)
	}
}

func TestCredentialsAreNotOnDiskInClear(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("lidl", map[string]string{"refresh_token": "hunter2-please-do-not-leak"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hunter2") {
		t.Fatal("the credential file contains the plaintext token")
	}

	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file mode = %o, want 600", perm)
	}
	keyInfo, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}
}

func TestMergeAndDelete(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("action", map[string]string{"email": "a@b.c", "password": "pw"}); err != nil {
		t.Fatal(err)
	}
	// Merge keeps untouched fields and an empty value removes one.
	if err := store.Merge("action", map[string]string{"token": "t", "password": ""}); err != nil {
		t.Fatal(err)
	}
	if store.Field("action", "email") != "a@b.c" {
		t.Error("merge dropped an untouched field")
	}
	if store.Field("action", "password") != "" {
		t.Error("merge with an empty value should delete the field")
	}
	if store.Field("action", "token") != "t" {
		t.Error("merge did not add the new field")
	}

	if err := store.Delete("action"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("action"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestWrongKeyIsReported(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("lidl", map[string]string{"refresh_token": "x"}); err != nil {
		t.Fatal(err)
	}

	// Simulate a key that no longer matches the credential file.
	t.Setenv(keyEnv, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if _, err := Open(dir); err == nil {
		t.Fatal("opening with the wrong key should fail loudly rather than silently losing credentials")
	}
}
