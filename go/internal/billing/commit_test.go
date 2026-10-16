package billing

import (
	"errors"
	"strings"
	"testing"
)

func TestCommitFailureRollsBack(t *testing.T) {
	b, _ := fixture(t)
	if e := b.Exec("CREATE TABLE review_deferred(id INTEGER REFERENCES keys(key_id) DEFERRABLE INITIALLY DEFERRED)"); e != nil {
		t.Fatal(e)
	}
	e := b.tx(func() error { _, e := b.exec("INSERT INTO review_deferred VALUES('missing')"); return e })
	if e == nil {
		t.Fatal("expected COMMIT failure")
	}
	t.Log("commit error:", e)
	e = b.Credit("test", 1)
	if e != nil {
		t.Fatal("credit after failed COMMIT:", e)
	}
	rows, e := b.rows("SELECT * FROM review_deferred")
	if e != nil || len(rows) != 0 {
		t.Fatalf("failed transaction persisted: %v %v", rows, e)
	}
}

func TestRollbackFailurePoisonsConnectionAndPreservesError(t *testing.T) {
	b, _ := fixture(t)
	original := errors.New("original transaction failure")
	err := b.tx(func() error {
		if _, e := b.exec("ROLLBACK"); e != nil {
			t.Fatal(e)
		}
		return original
	})
	if !errors.Is(err, original) {
		t.Fatalf("lost original error: %v", err)
	}
	if err := b.Credit("test", 1); err == nil || !strings.Contains(err.Error(), "poisoned") {
		t.Fatalf("uncertain connection reused: %v", err)
	}
	if _, err := b.rows("SELECT 1"); err == nil {
		t.Fatal("poisoned connection used for reads")
	}
}
