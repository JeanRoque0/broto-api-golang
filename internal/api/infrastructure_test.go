package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestConcurrentSessionRefresh(t *testing.T) {
	f := newFrontendHarness(t)
	id, token := f.account()
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 8)
	for i := 0; i < cap(results); i++ {
		go func() {
			<-start
			r := httptest.NewRequest("POST", "/v1/auth/refresh", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			f.h.ServeHTTP(w, r)
			results <- w
		}()
	}
	close(start)
	winners := 0
	var replacement string
	for i := 0; i < cap(results); i++ {
		w := <-results
		switch w.Code {
		case 200:
			winners++
			replacement = object(t, w.Body.Bytes())["access_token"].(string)
		case 401:
		default:
			t.Errorf("unexpected refresh response: %d %s", w.Code, w.Body)
		}
	}
	var count int
	if e := f.s.DB.QueryRow(context.Background(), "select count(*) from sessions where user_id=$1", id).Scan(&count); e != nil || count != 1 || winners != 1 {
		t.Fatalf("refresh winners=%d active sessions=%d error=%v", winners, count, e)
	}
	f.call("GET", "/v1/auth/user", token, nil, 401)
	f.call("GET", "/v1/auth/user", replacement, nil, 200)
}

func TestDatabaseUnavailableDoesNotInvalidateSession(t *testing.T) {
	f := newFrontendHarness(t)
	_, token := f.account()
	f.call("GET", "/v1/auth/user", token, nil, 200)
	// Close only this disposable fixture's pool; leave the running app untouched.
	f.s.DB.Close()
	f.call("GET", "/healthz", "", nil, 503)
	v := object(t, f.call("GET", "/v1/auth/user", token, nil, 503))
	if v["erro"] != "banco_indisponivel" {
		t.Fatal(v)
	}
	f.call("GET", "/v1/auth/user", "", nil, 401)
}

func TestStorageFailuresAndCleanupRetry(t *testing.T) {
	t.Run("unwritable destination", func(t *testing.T) {
		f := newFrontendHarness(t)
		_, token := f.account()
		blocker := filepath.Join(t.TempDir(), "file-instead-of-directory")
		if e := os.WriteFile(blocker, []byte("blocked"), 0600); e != nil {
			t.Fatal(e)
		}
		f.s.C.StorageDir = blocker
		f.call("POST", "/v1/photos", token, []byte{0xff, 0xd8, 0xff, 0x00}, 500)
		var count int
		if e := f.s.DB.QueryRow(context.Background(), "select count(*) from stored_files").Scan(&count); e != nil || count != 0 {
			t.Fatal("failed upload left metadata", count, e)
		}
	})
	t.Run("commit failure removes bytes", func(t *testing.T) {
		f := newFrontendHarness(t)
		id, token := f.account()
		// A deferred trigger fails COMMIT after the bytes were successfully written.
		f.exec(`create function fail_test_commit() returns trigger language plpgsql as $$
		begin raise exception 'injected commit failure'; end $$;
		create constraint trigger fail_test_commit after insert on stored_files
		deferrable initially deferred for each row execute function fail_test_commit()`)
		f.call("POST", "/v1/photos", token, []byte{0xff, 0xd8, 0xff, 0x00}, 500)
		entries, e := os.ReadDir(f.s.filePath(id))
		if e != nil || len(entries) != 0 {
			t.Fatal("failed commit left uploaded bytes", entries, e)
		}
		var count int
		if e := f.s.DB.QueryRow(context.Background(), "select count(*) from stored_files").Scan(&count); e != nil || count != 0 {
			t.Fatal("failed commit left metadata", count, e)
		}
	})
	t.Run("account deletion queues failed file removal", func(t *testing.T) {
		f := newFrontendHarness(t)
		id, token := f.account()
		path := f.photo(token)
		full := f.s.filePath(path)
		if e := os.Remove(full); e != nil {
			t.Fatal(e)
		}
		if e := os.Mkdir(full, 0700); e != nil {
			t.Fatal(e)
		}
		blocker := filepath.Join(full, "block-removal")
		if e := os.WriteFile(blocker, []byte("test"), 0600); e != nil {
			t.Fatal(e)
		}
		v := object(t, f.call("DELETE", "/v1/account", token, nil, 202))
		if v["photos_cleanup_pending"] != true {
			t.Fatal(v)
		}
		var users, queued int
		if e := f.s.DB.QueryRow(context.Background(), "select (select count(*) from users where id=$1),(select count(*) from file_deletions where path=$2)", id, path).Scan(&users, &queued); e != nil || users != 0 || queued != 1 {
			t.Fatal("account or queue state incorrect", users, queued, e)
		}
		f.call("GET", "/v1/auth/user", token, nil, 401)
		if e := os.Remove(blocker); e != nil {
			t.Fatal(e)
		}
		if e := f.s.drainFiles(context.Background()); e != nil {
			t.Fatal(e)
		}
		if _, e := os.Stat(full); !os.IsNotExist(e) {
			t.Fatal("cleanup retry did not remove file", e)
		}
		if e := f.s.DB.QueryRow(context.Background(), "select count(*) from file_deletions").Scan(&queued); e != nil || queued != 0 {
			t.Fatal("cleanup retry left queue entry", queued, e)
		}
	})
}

func TestScheduledJobLockAndRetry(t *testing.T) {
	f := newFrontendHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.s.runJob(ctx, "test-concurrent", func(ctx context.Context, tx pgx.Tx) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-started:
	case e := <-done:
		t.Fatal("job did not acquire lock", e)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	called := false
	competing := func(context.Context, pgx.Tx) error { called = true; return nil }
	e := f.s.runJob(ctx, "test-concurrent", competing)
	close(release)
	firstError := <-done
	if e != nil || firstError != nil || called {
		t.Fatal("concurrent job was not excluded", e, firstError, called)
	}
	if e = f.s.runJob(ctx, "test-concurrent", competing); e != nil || called {
		t.Fatal("completed job ran twice within the hour", e)
	}
	injected := errors.New("injected job failure")
	if e = f.s.runJob(ctx, "test-retry", func(context.Context, pgx.Tx) error { return injected }); !errors.Is(e, injected) {
		t.Fatal(e)
	}
	if e = f.s.runJob(ctx, "test-retry", competing); e != nil || !called {
		t.Fatal("failed job could not retry", e)
	}
}

func TestRunJobsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { (&Server{}).RunJobs(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
}
