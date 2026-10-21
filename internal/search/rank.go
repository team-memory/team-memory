package search

import (
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
)

// Ranking parameters. The formulas are documented in
// docs/decisions/retrieval-scoring.md; change both together.
const (
	// RRFK is the Reciprocal Rank Fusion constant from Cormack et al. (2009).
	RRFK = 60
	// BoostApproved and BoostCited are the multiplicative boosts for
	// decisions and conventions at those trust levels. BoostCap bounds the
	// total adjustment.
	BoostApproved = 0.10
	BoostCited    = 0.05
	BoostCap      = 0.10
	// PrecedenceMargin is the relative score band within which results from
	// more specific scopes are ordered ahead of inherited ones.
	PrecedenceMargin = 0.05
)

// Retriever names reported per result.
const (
	RetrieverFullText = "fulltext"
	RetrieverVector   = "vector"
)

// candidate is one memory's position in each retriever's list (1-based; 0
// means absent) plus the attributes ranking needs.
type candidate struct {
	id        uuid.UUID
	ftsRank   int
	vecRank   int
	vecSim    float64
	kind      memory.Kind
	trust     memory.Trust
	depth     int
	fused     float64 // RRF plus boost
	score     float64 // fused normalized to 0..1
	retriever []string
}

// rrf returns the Reciprocal Rank Fusion score of a candidate.
func rrf(c *candidate) float64 {
	s := 0.0
	if c.ftsRank > 0 {
		s += 1.0 / float64(RRFK+c.ftsRank)
	}
	if c.vecRank > 0 {
		s += 1.0 / float64(RRFK+c.vecRank)
	}
	return s
}

// boost is the bounded trust adjustment: approved or cited decisions and
// conventions get a small multiplicative lift, never more than BoostCap.
func boost(kind memory.Kind, trust memory.Trust) float64 {
	if kind != memory.KindDecision && kind != memory.KindConvention {
		return 0
	}
	b := 0.0
	switch trust {
	case memory.TrustApproved:
		b = BoostApproved
	case memory.TrustCited:
		b = BoostCited
	}
	return min(b, BoostCap)
}

// maxFused is the best possible fused score given which retrievers ran: rank
// 1 in each, with the maximum boost. Dividing by it maps scores into 0..1.
func maxFused(retrievers int) float64 {
	return float64(retrievers) / float64(RRFK+1) * (1 + BoostCap)
}

// rank fuses, scores and orders candidates. retrieversRun is the number of
// retrievers that actually ran (1 when vector search fell back).
func rank(cands []*candidate, retrieversRun int) []*candidate {
	norm := maxFused(max(retrieversRun, 1))
	for _, c := range cands {
		c.fused = rrf(c) * (1 + boost(c.kind, c.trust))
		c.score = min(c.fused/norm, 1)
		c.retriever = c.retriever[:0]
		if c.ftsRank > 0 {
			c.retriever = append(c.retriever, RetrieverFullText)
		}
		if c.vecRank > 0 {
			c.retriever = append(c.retriever, RetrieverVector)
		}
	}
	slices.SortFunc(cands, byScore)
	return applyPrecedence(cands)
}

func byScore(a, b *candidate) int {
	switch {
	case a.fused > b.fused:
		return -1
	case a.fused < b.fused:
		return 1
	}
	return strings.Compare(a.id.String(), b.id.String())
}

// applyPrecedence partitions the score-ordered list into bands: a band starts
// at its highest-scoring member and holds every following result whose score
// is within PrecedenceMargin of it. Inside a band, deeper (more specific)
// scopes come first, then score. Results never move across bands, so no
// result passes another whose score exceeds its own by more than the margin.
// Depth is ordering context only: it does not establish that two results
// conflict or that one has authority over the other.
func applyPrecedence(sorted []*candidate) []*candidate {
	for start := 0; start < len(sorted); {
		end := start + 1
		floor := sorted[start].fused / (1 + PrecedenceMargin)
		for end < len(sorted) && sorted[end].fused >= floor {
			end++
		}
		band := sorted[start:end]
		slices.SortStableFunc(band, func(a, b *candidate) int {
			if a.depth != b.depth {
				return b.depth - a.depth
			}
			return byScore(a, b)
		})
		start = end
	}
	return sorted
}
