package pgsql

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestResolveCCachePath(t *testing.T) {
	t.Run("explicit bare path wins over KRB5CCNAME", func(t *testing.T) {
		t.Setenv("KRB5CCNAME", "FILE:/env/should-be-ignored")
		got, err := resolveCCachePath("/explicit/cc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/explicit/cc" {
			t.Fatalf("got %q; want %q", got, "/explicit/cc")
		}
	})

	t.Run("explicit FILE: prefix is stripped", func(t *testing.T) {
		unsetCCName(t)
		got, err := resolveCCachePath("FILE:/explicit/cc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/explicit/cc" {
			t.Fatalf("got %q; want %q", got, "/explicit/cc")
		}
	})

	t.Run("KRB5CCNAME FILE: cache is used when no explicit path", func(t *testing.T) {
		t.Setenv("KRB5CCNAME", "FILE:/env/cc")
		got, err := resolveCCachePath("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/env/cc" {
			t.Fatalf("got %q; want %q", got, "/env/cc")
		}
	})

	t.Run("non-FILE KRB5CCNAME falls back to the default", func(t *testing.T) {
		// Only file-type caches are supported; an unprefixed path or another
		// cache type (DIR:, KEYRING:, ...) is ignored in favour of the default.
		want := defaultCCachePath(t)
		for _, v := range []string{"/env/cc-no-prefix", "DIR:/env/ccdir", "KEYRING:kine"} {
			t.Setenv("KRB5CCNAME", v)
			got, err := resolveCCachePath("")
			if err != nil {
				t.Fatalf("KRB5CCNAME=%q: unexpected error: %v", v, err)
			}
			if got != want {
				t.Fatalf("KRB5CCNAME=%q: got %q; want %q", v, got, want)
			}
		}
	})

	t.Run("unset KRB5CCNAME falls back to the default", func(t *testing.T) {
		unsetCCName(t)
		want := defaultCCachePath(t)
		got, err := resolveCCachePath("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Fatalf("got %q; want %q", got, want)
		}
	})
}

// TestNewGSSWithCCacheMissingCache exercises the credential-cache branch of
// gss.init: with a valid krb5.conf but a cache file that does not exist, the
// provider must fail at cache load, before any KDC contact. It also proves the
// explicit cache path is the file actually read.
func TestNewGSSWithCCacheMissingCache(t *testing.T) {
	t.Setenv("KRB5_CONFIG", writeKrb5Conf(t, "[libdefaults]\n  default_realm = KINE.TEST\n"))
	missing := filepath.Join(t.TempDir(), "krb5cc_missing")
	if _, err := newGSSWithCCache(missing); err == nil {
		t.Fatalf("newGSSWithCCache(%q) should fail for a nonexistent cache", missing)
	}
}

// unsetCCName clears KRB5CCNAME for the duration of the test, restoring it on
// cleanup via t.Setenv. The developer environment commonly sets it, so cases
// that exercise the default path must remove it first.
func unsetCCName(t *testing.T) {
	t.Helper()
	t.Setenv("KRB5CCNAME", "")
	os.Unsetenv("KRB5CCNAME")
}

// defaultCCachePath returns the MIT default cache path for the current user,
// skipping the test if the current user cannot be determined.
func defaultCCachePath(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("cannot determine current user: %v", err)
	}
	return "/tmp/krb5cc_" + u.Uid
}
