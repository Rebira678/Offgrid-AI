package eval

import (
	"testing"
)

func BenchmarkCasesFingerprint(b *testing.B) {
	cases, err := LoadCases()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CasesFingerprint(cases)
	}
}

func BenchmarkBaselineCompare(b *testing.B) {
	base, err := ReadBaseline("baseline.json")
	if err != nil {
		b.Fatal(err)
	}
	sums := make([]Summary, len(base.Setups))
	for i, s := range base.Setups {
		sums[i] = Summary{
			UseSchema:         s.UseSchema,
			MaxAttempts:       s.MaxAttempts,
			Cases:             s.Cases,
			ValidFirstTry:     s.ValidFirstTry,
			Succeeded:         s.Succeeded,
			IntentCorrect:     s.IntentCorrect,
			ArgsChecked:       s.ArgsChecked,
			ArgsCorrect:       s.ArgsCorrect,
			PromptVersion:     base.PromptVersion,
			PromptFingerprint: base.PromptFingerprint,
			CasesFingerprint:  base.CasesFingerprint,
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := base.Compare(sums, 0.02)
		if err != nil {
			b.Fatal(err)
		}
	}
}
