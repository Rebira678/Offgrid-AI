package sqlstore

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func BenchmarkCreateInvite(b *testing.B) {
	s, _, _ := admin(&testing.T{})
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := s.CreateInvite(ctx, RoleMember, "bench-device", 15*time.Minute)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMembershipFold100(b *testing.B) {
	events := make([]memberEvent, 100)
	for i := 0; i < 100; i++ {
		role := RoleMember
		if i == 0 {
			role = RoleAdmin
		}
		events[i] = memberEvent{
			Node:    "node",
			Seq:     int64(i + 1),
			TS:      time.Unix(int64(1700000000+i), 0).UTC(),
			Counter: int64(i + 1),
			Signer:  fmt.Sprintf("dev_%032d", 0),
			Add: memberAddPayload{
				Device:    fmt.Sprintf("dev_%032d", i),
				PublicKey: fmt.Sprintf("key_%032d", i),
				Role:      role,
				Label:     fmt.Sprintf("device-%d", i),
			},
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		foldMembership(events)
	}
}
