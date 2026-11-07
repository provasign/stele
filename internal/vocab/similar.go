package vocab

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// SimilarPairs proposes pairs of attributes that may be one fact: each
// attribute's nearest neighbours by TF-IDF cosine over its name, meaning and
// the text of the requirements that use it. Splitting one fact across
// attributes is the vocabulary's main failure, and a model asked about one
// long list finds few of them; a short list of close pairs it can judge.
func SimilarPairs(used []Used, k int, threshold float64) [][2]int {
	docs := make([]map[string]float64, len(used))
	df := map[string]int{}
	for i, u := range used {
		tf := map[string]float64{}
		_, attr, _ := strings.Cut(u.Name, ".")
		for _, w := range translate.Words(attr + " " + u.Name + " " + u.Meaning) {
			tf[w] += 2 // the symbol's own words weigh more than its statements'
		}
		for _, w := range translate.Words(u.Text) {
			tf[w]++
		}
		for w := range tf {
			df[w]++
		}
		docs[i] = tf
	}
	n := float64(len(used))
	norm := make([]float64, len(used))
	for i, tf := range docs {
		for w, f := range tf {
			tf[w] = (1 + math.Log(f)) * math.Log(1+n/float64(df[w]))
			norm[i] += tf[w] * tf[w]
		}
		norm[i] = math.Sqrt(norm[i])
	}
	cos := func(i, j int) float64 {
		a, b := docs[i], docs[j]
		if len(a) > len(b) {
			a, b = b, a
		}
		dot := 0.0
		for w, x := range a {
			dot += x * b[w]
		}
		if norm[i] == 0 || norm[j] == 0 {
			return 0
		}
		return dot / (norm[i] * norm[j])
	}
	seen := map[[2]int]bool{}
	var out [][2]int
	for i := range used {
		type nb struct {
			j int
			s float64
		}
		var nbs []nb
		for j := range used {
			if j != i {
				if s := cos(i, j); s >= threshold {
					nbs = append(nbs, nb{j, s})
				}
			}
		}
		sort.Slice(nbs, func(a, b int) bool { return nbs[a].s > nbs[b].s })
		for _, x := range nbs[:min(k, len(nbs))] {
			p := [2]int{min(i, x.j), max(i, x.j)}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a][0] < out[b][0] || out[a][0] == out[b][0] && out[a][1] < out[b][1] })
	return out
}

// Link is a world fact relating two attributes of a candidate pair: what
// holds because of what the two things are (one lives inside the other, one
// exists only while the other does, one is defined by the other), never
// what a requirement wants. A, B name the pair's attributes.
type Link struct {
	Pair   int    `json:"pair"`
	When   string `json:"when"`
	Assert string `json:"assert"`
	Reason string `json:"reason"`
	A, B   string `json:"-"`
}

// ConsolidatePairs asks, batch by batch, about each candidate pair: one fact
// under two names (a merge), two facts one of which constrains the other
// by their nature (a link), or neither. Up to parallel batches are asked at
// once; with more than one, the pairs are spread so every call gets a share,
// down to 10 pairs per call. Answers are combined in pair order.
func ConsolidatePairs(ctx context.Context, m llm.Client, base *ir.Registry, used []Used, pairs [][2]int, batch, parallel int, rep *Report) ([]Merge, []Link, error) {
	if batch <= 0 {
		batch = 60
	}
	parallel = max(1, parallel)
	if parallel > 1 && len(pairs) > 0 {
		batch = min(batch, max(10, (len(pairs)+parallel-1)/parallel))
	}
	desc := func(u Used) string {
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s", u.Name, u.Type)
		if u.Meaning != "" {
			fmt.Fprintf(&b, " - %s", u.Meaning)
		}
		for _, x := range u.Examples {
			fmt.Fprintf(&b, "\n      used by: %s", x)
		}
		return b.String()
	}
	type answer struct {
		reply *llm.Reply
		err   error
	}
	var starts []int
	for i := 0; i < len(pairs); i += batch {
		starts = append(starts, i)
	}
	answers := make([]answer, len(starts))
	system := consolidatePrompt + translate.RenderVocabulary(base)
	// Every batch has this system prompt: one call that asks about no pair
	// writes it to the cache for the batches (llm.Warm). Sent together, the
	// four to six batches of a 60-requirement run each wrote 13,000 to
	// 20,000 tokens of it, a quarter of the run's cost (2026-11-06).
	if r := llm.Warm(ctx, m, len(starts), llm.Request{System: system, Turns: []llm.Turn{{Role: "user", Text: "There are no pairs in this request: return both lists empty."}}, Schema: pairsSchema}); r != nil {
		rep.Usage.Add(r.Usage)
	}
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for k, i := range starts {
		// Each attribute is described once, and the pairs name them: an
		// attribute is in several pairs, and its description and examples
		// were repeated in each, most of a call's prompt (2026-11-06).
		var req strings.Builder
		req.WriteString(pairsRequest)
		req.WriteString("Attributes:\n\n")
		described := map[int]bool{}
		for _, p := range pairs[i:min(i+batch, len(pairs))] {
			for _, x := range p {
				if !described[x] {
					described[x] = true
					fmt.Fprintf(&req, "- %s\n", desc(used[x]))
				}
			}
		}
		req.WriteString("\nPairs:\n\n")
		for j, p := range pairs[i:min(i+batch, len(pairs))] {
			fmt.Fprintf(&req, "pair %d: A %s, B %s\n", i+j+1, used[p[0]].Name, used[p[1]].Name)
		}
		wg.Add(1)
		go func(k int, text string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			answers[k].reply, answers[k].err = m.Complete(ctx, llm.Request{
				System: system,
				Turns:  []llm.Turn{{Role: "user", Text: text}},
				Schema: pairsSchema,
			})
		}(k, req.String())
	}
	wg.Wait()
	var merges []Merge
	var links []Link
	for k, i := range starts {
		reply, err := answers[k].reply, answers[k].err
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("pairs %d-%d: %v", i+1, min(i+batch, len(pairs)), err))
			continue
		}
		rep.Rounds++
		rep.Usage.Add(reply.Usage)
		var out struct {
			Merges []Merge `json:"merges"`
			Links  []Link  `json:"links"`
		}
		if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("pairs %d-%d: reply is not the expected JSON: %v", i+1, min(i+batch, len(pairs)), err))
			continue
		}
		merges = append(merges, out.Merges...)
		for _, l := range out.Links {
			if l.Pair < 1 || l.Pair > len(pairs) || l.Assert == "" {
				continue
			}
			p := pairs[l.Pair-1]
			l.A, l.B = used[p[0]].Name, used[p[1]].Name
			links = append(links, l)
		}
	}
	return merges, links, nil
}

const pairsRequest = `Each pair below holds two attributes the translated requirements use; the attributes are described first, with requirements that use them. Requirements can only contradict each other through facts they share, so decide for each pair which of these it is:

- The same fact under two names: the same thing about the same object, perhaps seen from two components, as a flag on one entity and an enum value on another, or one the negation of the other. Return a merge of the narrower or less descriptive attribute into the other. Merge only when every requirement using the source can be stated with the target.
- Two facts, one of which constrains the other by what the things are: one lives inside or is part of the other (a process's environment is part of its machine's environment), one exists only while the other does (files on a machine's disk survive only while the machine exists), one is defined by the other. Return a link: an invariant over the two attributes that holds in every state, in "assert" (with "when" empty, or holding the condition under which it applies). Write paths from each entity's root in lower camel case (order.status, paymentRecord.captured), with =, !=, in {..}, not, and, or, implies. A link states how the world is, never what a requirement wants: if it holds only because some requirement says so, it is not a link.
- Neither: return nothing for the pair.

Most pairs are neither. A wrong merge or link invents contradictions; a missing one hides them.

`

const pairsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["merges", "links"],
  "properties": {
    "merges": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["from", "into", "reason"],
        "properties": {"from": {"type": "string"}, "into": {"type": "string"}, "reason": {"type": "string"}}
      }
    },
    "links": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["pair", "when", "assert", "reason"],
        "properties": {"pair": {"type": "integer"}, "when": {"type": "string"}, "assert": {"type": "string"}, "reason": {"type": "string"}}
      }
    }
  }
}`
