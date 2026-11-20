package deviceplugin

import (
	"slices"

	"github.com/kybernate/kybernate/internal/inventory"
)

// Scores of the connection between two GPUs, following the order of
// `nvidia-smi topo -m`. Every NVLink counts on its own, so two GPUs with more
// links between them score higher.
const (
	scorePerNVLink   = 100
	scorePCIeSwitch  = 40
	scoreHostBridge  = 20
	scoreSystem      = 10
	scoreP2PBonus    = 5
	maxCombinations  = 20000
	unknownDeviceIdx = 1 << 30
)

// combinationLimit caps the exhaustive search in Prefer. Tests lower it to
// exercise the greedy fallback.
var combinationLimit = maxCombinations

// LinkScore rates the connection to another GPU. A higher score is faster.
func LinkScore(link inventory.Link) int {
	var score int
	switch link.Type {
	case inventory.LinkNVLink:
		return scorePerNVLink * max(link.NVLinks, 1)
	case inventory.LinkPCIeSwitch:
		score = scorePCIeSwitch
	case inventory.LinkPCIeHostBridge:
		score = scoreHostBridge
	default:
		score = scoreSystem
	}
	if link.P2P {
		score += scoreP2PBonus
	}
	return score
}

// Prefer picks size GPUs out of available, including all of mustInclude, so
// that the chosen GPUs are connected as well as possible among each other: the
// sum of the scores between every pair of chosen GPUs is the highest.
//
// Among equally good choices, Prefer takes the one whose GPUs are connected
// the least to the GPUs left over. This keeps well connected GPUs together for
// later pods: a pod with one GPU gets a GPU whose NVLink peer is already in
// use, if there is one. Remaining ties go to the lowest GPU index.
//
// The search is exhaustive up to maxCombinations candidates and greedy beyond.
// The result is ordered like devices. Reserved and unknown GPUs are skipped.
func Prefer(devices []Device, available, mustInclude []string, size int) []string {
	order := make(map[string]int, len(devices))
	links := make(map[string]map[string]int, len(devices))
	reserved := make(map[string]bool)
	for i, d := range devices {
		order[d.ID] = i
		links[d.ID] = d.Links
		if d.Reserved {
			reserved[d.ID] = true
		}
	}
	score := func(a, b string) int {
		return max(links[a][b], links[b][a])
	}
	position := func(id string) int {
		if i, ok := order[id]; ok {
			return i
		}
		return unknownDeviceIdx
	}
	sortByPosition := func(ids []string) {
		slices.SortStableFunc(ids, func(a, b string) int { return position(a) - position(b) })
	}

	var must []string
	for _, id := range mustInclude {
		if !slices.Contains(must, id) {
			must = append(must, id)
		}
	}
	var candidates []string
	for _, id := range available {
		if _, ok := order[id]; !ok || reserved[id] || slices.Contains(must, id) || slices.Contains(candidates, id) {
			continue
		}
		candidates = append(candidates, id)
	}
	sortByPosition(candidates)

	need := size - len(must)
	switch {
	case need <= 0:
		out := slices.Clone(must)
		sortByPosition(out)
		return out
	case need >= len(candidates):
		out := slices.Concat(must, candidates)
		sortByPosition(out)
		return out
	}

	var chosen []string
	if binomial(len(candidates), need) <= combinationLimit {
		chosen = bestCombination(must, candidates, need, score)
	} else {
		chosen = greedy(must, candidates, need, score)
	}
	out := slices.Concat(must, chosen)
	sortByPosition(out)
	return out
}

// bestCombination tries every combination of need candidates.
func bestCombination(must, candidates []string, need int, score func(a, b string) int) []string {
	idx := make([]int, need)
	for i := range idx {
		idx[i] = i
	}
	var best []int
	bestInner, bestOuter := -1, 0
	set := make([]string, 0, len(must)+need)
	picked := make([]bool, len(candidates))
	for {
		set = append(set[:0], must...)
		clear(picked)
		for _, i := range idx {
			set = append(set, candidates[i])
			picked[i] = true
		}
		inner := 0
		for a := range set {
			for b := a + 1; b < len(set); b++ {
				inner += score(set[a], set[b])
			}
		}
		outer := 0
		for i, c := range candidates {
			if picked[i] {
				continue
			}
			for _, s := range set {
				outer += score(s, c)
			}
		}
		if inner > bestInner || (inner == bestInner && outer < bestOuter) {
			best, bestInner, bestOuter = slices.Clone(idx), inner, outer
		}

		// Next combination in lexicographic order.
		i := need - 1
		for i >= 0 && idx[i] == len(candidates)-need+i {
			i--
		}
		if i < 0 {
			break
		}
		idx[i]++
		for j := i + 1; j < need; j++ {
			idx[j] = idx[j-1] + 1
		}
	}
	out := make([]string, 0, need)
	for _, i := range best {
		out = append(out, candidates[i])
	}
	return out
}

// greedy adds one candidate at a time, the one best connected to the GPUs
// chosen so far. Without any GPU chosen yet, it starts with the candidate best
// connected to all others.
func greedy(must, candidates []string, need int, score func(a, b string) int) []string {
	set := slices.Clone(must)
	rest := slices.Clone(candidates)
	var out []string
	for len(out) < need {
		reference := set
		if len(reference) == 0 {
			reference = rest
		}
		bestIdx, bestScore := 0, -1
		for i, c := range rest {
			s := 0
			for _, r := range reference {
				if r != c {
					s += score(c, r)
				}
			}
			if s > bestScore {
				bestIdx, bestScore = i, s
			}
		}
		set = append(set, rest[bestIdx])
		out = append(out, rest[bestIdx])
		rest = slices.Delete(rest, bestIdx, bestIdx+1)
	}
	return out
}

// binomial returns n over k, capped above maxCombinations to avoid overflow.
func binomial(n, k int) int {
	k = min(k, n-k)
	result := 1
	for i := 1; i <= k; i++ {
		result = result * (n - k + i) / i
		if result > maxCombinations*10 {
			return result
		}
	}
	return result
}
