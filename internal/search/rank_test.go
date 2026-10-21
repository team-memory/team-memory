package search

import (
	"fmt"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
)

// testNames maps candidate ids back to the names tests gave them.
var testNames = map[uuid.UUID]string{}

func cand(name string, fts, vec int, depth int, kind memory.Kind, trust memory.Trust) *candidate {
	id := uuid.NewSHA1(uuid.Nil, []byte(name))
	testNames[id] = name
	return &candidate{id: id, ftsRank: fts, vecRank: vec, depth: depth, kind: kind, trust: trust}
}

func names(cs []*candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = testNames[c.id]
	}
	return out
}

func TestRRFFavoursAgreement(t *testing.T) {
	both := cand("aa", 3, 3, 0, memory.KindFact, memory.TrustUnverified)
	ftsOnly := cand("ba", 1, 0, 0, memory.KindFact, memory.TrustUnverified)
	vecOnly := cand("ca", 0, 1, 0, memory.KindFact, memory.TrustUnverified)
	out := rank([]*candidate{ftsOnly, vecOnly, both}, 2)
	if got := names(out); got[0] != "aa" {
		t.Fatalf("result in both lists should rank first: %v", got)
	}
	if !slicesEqual(out[0].retriever, []string{RetrieverFullText, RetrieverVector}) {
		t.Fatalf("retrievers: %v", out[0].retriever)
	}
}

func TestScoreBoundsAndMonotonic(t *testing.T) {
	best := cand("aa", 1, 1, 0, memory.KindDecision, memory.TrustApproved)
	worse := cand("ba", 2, 5, 0, memory.KindDecision, memory.TrustApproved)
	out := rank([]*candidate{worse, best}, 2)
	if math.Abs(out[0].score-1) > 1e-9 {
		t.Fatalf("rank 1 in both lists with max boost should score 1, got %v", out[0].score)
	}
	if !(out[1].score < out[0].score && out[1].score > 0) {
		t.Fatalf("scores not monotonic in 0..1: %v %v", out[0].score, out[1].score)
	}
	// Full-text-only fallback normalizes by one retriever.
	solo := cand("ca", 1, 0, 0, memory.KindFact, memory.TrustUnverified)
	out = rank([]*candidate{solo}, 1)
	if want := 1 / (1 + BoostCap); math.Abs(out[0].score-want) > 1e-9 {
		t.Fatalf("fallback score: got %v want %v", out[0].score, want)
	}
}

func TestBoostIsBounded(t *testing.T) {
	for _, k := range memory.Kinds {
		for _, tr := range []memory.Trust{memory.TrustUnverified, memory.TrustCited, memory.TrustApproved} {
			if b := boost(k, tr); b < 0 || b > BoostCap {
				t.Fatalf("boost(%s,%s)=%v outside [0,%v]", k, tr, b, BoostCap)
			}
		}
	}
	if boost(memory.KindFact, memory.TrustApproved) != 0 {
		t.Fatal("facts are not boosted")
	}
	if boost(memory.KindConvention, memory.TrustCited) != BoostCited || boost(memory.KindDecision, memory.TrustApproved) != BoostApproved {
		t.Fatal("decision/convention boosts")
	}
	// An approved decision at rank 2 overtakes an unverified one at rank 1 in
	// the same single list, but not one that both retrievers agree on.
	a := cand("aa", 1, 0, 0, memory.KindDecision, memory.TrustUnverified)
	b := cand("ba", 2, 0, 0, memory.KindDecision, memory.TrustApproved)
	c := cand("ca", 3, 3, 0, memory.KindFact, memory.TrustUnverified)
	out := rank([]*candidate{a, b, c}, 2)
	if got := names(out); got[0] != "ca" || got[1] != "ba" || got[2] != "aa" {
		t.Fatalf("order: %v", got)
	}
}

func TestPrecedenceWithinMarginOnly(t *testing.T) {
	// Nearly tied: the project (depth 2) result moves ahead of the org one.
	org := cand("aa", 1, 0, 0, memory.KindConvention, memory.TrustUnverified)
	proj := cand("ba", 2, 0, 2, memory.KindConvention, memory.TrustUnverified)
	out := rank([]*candidate{org, proj}, 1)
	if got := names(out); got[0] != "ba" {
		t.Fatalf("within margin, the more specific scope should lead: %v", got)
	}
	// Far apart: relevance wins regardless of depth.
	org = cand("aa", 1, 1, 0, memory.KindConvention, memory.TrustUnverified)
	proj = cand("ba", 10, 0, 2, memory.KindConvention, memory.TrustUnverified)
	out = rank([]*candidate{org, proj}, 2)
	if got := names(out); got[0] != "aa" {
		t.Fatalf("outside margin, score must win: %v", got)
	}
	// No result passes one that out-scores it by more than the margin, even
	// when every lower-scoring result comes from a deeper scope.
	var cs []*candidate
	for i := 1; i <= 30; i++ {
		cs = append(cs, cand(fmt.Sprint("r", i), i, 0, i, memory.KindFact, memory.TrustUnverified))
	}
	out = rank(cs, 1)
	if names(out)[0] == "r1" {
		t.Fatal("banding should reorder near-ties toward deeper scopes")
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].fused > out[i].fused*(1+PrecedenceMargin) {
				t.Fatalf("result %d (%.5f) placed above %d (%.5f) beyond the margin", i, out[i].fused, j, out[j].fused)
			}
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
