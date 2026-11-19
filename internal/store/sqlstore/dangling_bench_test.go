package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/task"
)

func BenchmarkTaskIsVoid_FastPathEmpty(b *testing.B) {
	ctx := context.Background()
	e := trioBench(b)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		isVoid, err := taskIsVoid(ctx, e.a.db, "")
		if err != nil || isVoid {
			b.Fatal(isVoid, err)
		}
	}
}

func BenchmarkTaskIsVoid_NonExistent(b *testing.B) {
	ctx := context.Background()
	e := trioBench(b)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		isVoid, err := taskIsVoid(ctx, e.a.db, "T99999.laptop")
		if err != nil || isVoid {
			b.Fatal(isVoid, err)
		}
	}
}

func BenchmarkTaskIsVoid_LiveTask(b *testing.B) {
	ctx := context.Background()
	e := trioBench(b)
	tk, _, err := e.a.Create(ctx, task.NewTask{Title: "live task", Priority: task.PriorityMedium})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		isVoid, err := taskIsVoid(ctx, e.a.db, tk.ID)
		if err != nil || isVoid {
			b.Fatal(isVoid, err)
		}
	}
}

func BenchmarkSuggestionIsVoid_FastPathInvalid(b *testing.B) {
	ctx := context.Background()
	e := trioBench(b)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		isVoid, err := suggestionIsVoid(ctx, e.a.db, "", 0)
		if err != nil || isVoid {
			b.Fatal(isVoid, err)
		}
	}
}

func BenchmarkRefuseIfRevoked_FastPathNoRevokes(b *testing.B) {
	ctx := context.Background()
	e := trioBench(b)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := e.a.write(ctx, func(tx *sql.Tx) error {
			return e.a.refuseIfRevoked(ctx, tx, KindTaskCreated, 100)
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDanglingClosure(b *testing.B) {
	ctx := context.Background()
	corrupt := errors.New("corrupt")
	fn := func() (bool, error) { return true, nil }

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := dangling(ctx, nil, corrupt, fn); err != nil {
			b.Fatal(err)
		}
	}
}

func trioBench(b *testing.B) *trioEnv {
	b.Helper()
	ctx := context.Background()
	e := &trioEnv{ida: newKeyTB(b), idb: newKeyTB(b), idc: newKeyTB(b)}
	e.a, e.clkA = memberTB(b, "laptop", e.ida)
	e.b, e.clkB = memberTB(b, "pi", e.idb)
	e.c, _ = memberTB(b, "tab", e.idc)
	if err := e.a.Found(ctx, "founder"); err != nil {
		b.Fatal(err)
	}
	e.clkA.tick(time.Second)
	if err := e.a.AddMember(ctx, e.idb.PublicKey(), RoleMember, "pi"); err != nil {
		b.Fatal(err)
	}
	e.clkA.tick(time.Second)
	if err := e.a.AddMember(ctx, e.idc.PublicKey(), RoleMember, "tab"); err != nil {
		b.Fatal(err)
	}
	pullTB(b, e.b, e.a, 50)
	pullTB(b, e.c, e.a, 50)
	return e
}

func newKeyTB(tb testing.TB) *identity.Identity {
	tb.Helper()
	k, err := identity.Generate()
	if err != nil {
		tb.Fatal(err)
	}
	return k
}

func memberTB(tb testing.TB, node string, id *identity.Identity) (*Store, *clock) {
	tb.Helper()
	clk := &clock{t: epoch}
	s, err := Open(context.Background(), tb.TempDir()+"/"+node+".db", Options{
		NodeID: node, Now: clk.now, Signer: id, RequireSigned: true,
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	return s, clk
}

func pullTB(tb testing.TB, dst, src *Store, limit int) {
	tb.Helper()
	ctx := context.Background()
	sum, err := dst.Summary(ctx)
	if err != nil {
		tb.Fatal(err)
	}
	ops, _, err := src.OpsSince(ctx, sum, limit)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := dst.ApplyRemote(ctx, ops); err != nil {
		tb.Fatal(err)
	}
}
