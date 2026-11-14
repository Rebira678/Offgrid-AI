package sqlstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/task"
)

func BenchmarkAuthorize(b *testing.B) {
	founder, _ := identity.Generate()
	worker, _ := identity.Generate()

	events := []memberEvent{
		{
			Kind:    KindMemberAdd,
			Node:    "n-admin",
			Seq:     1,
			TS:      time.Unix(1700000000, 0).UTC(),
			Counter: 1,
			Signer:  founder.ID(),
			Add: memberAddPayload{
				Device:    founder.ID(),
				PublicKey: founder.PublicKey(),
				Role:      RoleAdmin,
				Label:     "founder",
			},
		},
		{
			Kind:    KindMemberAdd,
			Node:    "n-admin",
			Seq:     2,
			TS:      time.Unix(1700000010, 0).UTC(),
			Counter: 2,
			Signer:  founder.ID(),
			Add: memberAddPayload{
				Device:    worker.ID(),
				PublicKey: worker.PublicKey(),
				Role:      RoleMember,
				Label:     "worker",
			},
		},
		{
			Kind:    KindMemberRevoke,
			Node:    "n-admin",
			Seq:     3,
			TS:      time.Unix(1700000100, 0).UTC(),
			Counter: 3,
			Signer:  founder.ID(),
			Revoke: memberRevokePayload{
				Device: worker.ID(),
				Cutoffs: []Cutoff{
					{Node: "n-worker", Seq: 10, Hash: strings.Repeat("a", 64)},
				},
			},
		},
	}
	st := foldState(events, nil)
	opAllowed := OpRef{Node: "n-worker", Seq: 5, Kind: "task.create", Signer: worker.ID()}
	opRevoked := OpRef{Node: "n-worker", Seq: 15, Kind: "task.create", Signer: worker.ID()}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			_ = Authorize(st, opAllowed)
		} else {
			_ = Authorize(st, opRevoked)
		}
	}
}

func BenchmarkAuthorize1000(b *testing.B) {
	founder, _ := identity.Generate()
	worker, _ := identity.Generate()

	events := []memberEvent{
		{
			Kind:    KindMemberAdd,
			Node:    "n-admin",
			Seq:     1,
			TS:      time.Unix(1700000000, 0).UTC(),
			Counter: 1,
			Signer:  founder.ID(),
			Add: memberAddPayload{
				Device:    founder.ID(),
				PublicKey: founder.PublicKey(),
				Role:      RoleAdmin,
				Label:     "founder",
			},
		},
		{
			Kind:    KindMemberAdd,
			Node:    "n-admin",
			Seq:     2,
			TS:      time.Unix(1700000010, 0).UTC(),
			Counter: 2,
			Signer:  founder.ID(),
			Add: memberAddPayload{
				Device:    worker.ID(),
				PublicKey: worker.PublicKey(),
				Role:      RoleMember,
				Label:     "worker",
			},
		},
		{
			Kind:    KindMemberRevoke,
			Node:    "n-admin",
			Seq:     3,
			TS:      time.Unix(1700000100, 0).UTC(),
			Counter: 3,
			Signer:  founder.ID(),
			Revoke: memberRevokePayload{
				Device: worker.ID(),
				Cutoffs: []Cutoff{
					{Node: "n-worker", Seq: 500, Hash: strings.Repeat("b", 64)},
				},
			},
		},
	}
	st := foldState(events, nil)

	ops := make([]OpRef, 1000)
	for i := 0; i < 1000; i++ {
		ops[i] = OpRef{
			Node:   "n-worker",
			Seq:    int64(i + 1),
			Kind:   "task.create",
			Signer: worker.ID(),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 1000; j++ {
			_ = Authorize(st, ops[j])
		}
	}
}

func BenchmarkRevoke(b *testing.B) {
	ctx := context.Background()
	s, _, _ := admin(&testing.T{})
	defer s.Close()

	type pair struct {
		id string
	}
	members := make([]pair, b.N)
	for i := 0; i < b.N; i++ {
		k, err := identity.Generate()
		if err != nil {
			b.Fatal(err)
		}
		if err := s.AddMember(ctx, k.PublicKey(), RoleMember, fmt.Sprintf("worker-%d", i)); err != nil {
			b.Fatal(err)
		}
		members[i] = pair{id: k.ID()}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Revoke(ctx, members[i].id); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRefoldView(b *testing.B) {
	ctx := context.Background()
	s, _, _ := admin(&testing.T{})
	defer s.Close()

	workerKey, _ := identity.Generate()
	err := s.AddMember(ctx, workerKey.PublicKey(), RoleMember, "bench-worker")
	if err != nil {
		b.Fatal(err)
	}

	// Create 20 tasks
	for i := 0; i < 20; i++ {
		_, _, err := s.Create(ctx, task.NewTask{
			Title:    fmt.Sprintf("bench task %d", i),
			Site:     "site-1",
			Priority: task.PriorityMedium,
		})
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			b.Fatal(err)
		}
		if err := refoldView(ctx, tx); err != nil {
			tx.Rollback()
			b.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			b.Fatal(err)
		}
	}
}
