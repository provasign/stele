package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/review"
	"github.com/provasign/stele/internal/translate"
)

// cmdBridge works backward from suspected contradictions: for each pair of
// statements a review cites, it shows the model their rules and asks for
// the world facts that connect them (a resource two things share, a part
// and its whole, two names for one thing). A contradiction that needs such
// a fact is invisible to the checker until it is stated. The facts are
// added, each only if the registry still loads; the checker then decides
// which candidates really conflict, and `stele verify` judges what it finds.
func cmdBridge(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("bridge", flag.ExitOnError)
	regDir := fs.String("registry", "", "formalized registry directory")
	stmtsPath := fs.String("statements", "", "the statements file it was formalized from")
	reviewPath := fs.String("review", "", "candidate contradictions (stele review --out)")
	outDir := fs.String("out", "", "directory for the registry with the facts added")
	batch := fs.Int("batch", 6, "candidates per request")
	parallel := fs.Int("parallel", 4, "concurrent requests")
	requestTimeout := fs.Duration("request-timeout", 10*time.Minute, "limit per model call")
	client := llmFlags(fs)
	_ = fs.Parse(args)
	if *regDir == "" || *stmtsPath == "" || *reviewPath == "" || *outDir == "" {
		fmt.Fprint(os.Stderr, "usage: stele bridge --registry DIR --statements FILE --review review.json --out DIR [--llm B]\n")
		return 2
	}
	reg, err := ir.LoadDir(*regDir)
	if err != nil {
		return failf("bridge", err)
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("bridge", err)
	}
	src, err := os.ReadFile(*reviewPath)
	if err != nil {
		return failf("bridge", err)
	}
	var rev review.Result
	if err := json.Unmarshal(src, &rev); err != nil {
		return failf("bridge", err)
	}
	m, err := client()
	if err != nil {
		return failf("bridge", err)
	}
	prose := map[string]string{}
	for _, s := range stmts {
		prose[s.ID] = strings.Join(strings.Fields(s.Prose), " ")
	}
	rulesOf := map[string][]*ir.Rule{}
	for _, x := range reg.Rules {
		if x.Live() && x.StatementID != "" {
			rulesOf[x.StatementID] = append(rulesOf[x.StatementID], x)
		}
	}
	// The candidates whose statements all have rules to connect.
	var items []review.Finding
	for _, f := range rev.Findings {
		ok := len(f.StatementIDs) >= 2
		for _, id := range f.StatementIDs {
			ok = ok && len(rulesOf[id]) > 0
		}
		if ok {
			items = append(items, f)
		}
	}
	render := func(k int, f review.Finding) string {
		var b strings.Builder
		fmt.Fprintf(&b, "candidate %d: %s\n", k, f.Situation)
		for _, id := range f.StatementIDs {
			fmt.Fprintf(&b, "statement %s: %s\n", id, truncate(prose[id], 400))
			for _, x := range rulesOf[id] {
				fmt.Fprintf(&b, "  rule %s: %s\n", x.ID, ruleText(x))
			}
		}
		return b.String()
	}
	system := "Stele describes a software system by a vocabulary and checks requirements over it. The vocabulary:\n\n" +
		translate.RenderVocabulary(reg) + "\n" + ruleLanguage() + bridgeInstructions
	start := time.Now()
	var facts []worldFact
	var world []string // attributes the world decides
	var usage llm.Usage
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, *parallel)
	var wg sync.WaitGroup
	for lo := 0; lo < len(items); lo += *batch {
		hi := min(lo+*batch, len(items))
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var req strings.Builder
			for k := lo; k < hi; k++ {
				req.WriteString(render(k+1, items[k]))
				req.WriteString("\n")
			}
			cctx, cancel := context.WithTimeout(context.Background(), *requestTimeout)
			reply, err := m.Complete(cctx, llm.Request{System: system, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: bridgeSchema})
			cancel()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			usage.Add(reply.Usage)
			var got struct {
				Facts []struct{ Assert, Reason string }    `json:"facts"`
				World []struct{ Attribute, Reason string } `json:"world"`
			}
			if err := json.Unmarshal([]byte(reply.JSON), &got); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, w := range got.World {
				if a := strings.TrimSpace(w.Attribute); a != "" && !slices.Contains(world, a) {
					world = append(world, a)
				}
			}
			for _, f := range got.Facts {
				if a := strings.TrimSpace(f.Assert); a != "" {
					facts = append(facts, worldFact{Assert: a, Reason: f.Reason})
				}
			}
		}(lo, hi)
	}
	wg.Wait()
	if firstErr != nil {
		return failf("bridge", firstErr)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return failf("bridge", err)
	}
	if _, err := carryOver(*regDir, *outDir, 0, nil, nil, nil); err != nil {
		return failf("bridge", err)
	}
	setWorld, err := worldSet(*outDir, world)
	if err != nil {
		return failf("bridge", err)
	}
	facts = dedupeFacts(facts)
	round := nextFactRound(*outDir)
	kept, rejected, err := addFactsReport(*outDir, round, facts)
	if err != nil {
		return failf("bridge", err)
	}
	// One repair: show the model each fact that did not load and why.
	if len(rejected) > 0 {
		var req strings.Builder
		req.WriteString("These world facts did not load. Rewrite each over the declared attributes and relations (or leave it out if the vocabulary cannot state it):\n\n")
		for _, f := range rejected {
			fmt.Fprintf(&req, "fact: %s\nreason: %s\nerror: %s\n\n", f.Assert, f.Reason, truncate(lastLine(f.Error), 300))
		}
		cctx, cancel := context.WithTimeout(context.Background(), *requestTimeout)
		reply, err := m.Complete(cctx, llm.Request{System: system, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: bridgeSchema})
		cancel()
		if err == nil {
			usage.Add(reply.Usage)
			var got struct {
				Facts []struct{ Assert, Reason string } `json:"facts"`
			}
			if json.Unmarshal([]byte(reply.JSON), &got) == nil {
				all := slices.Clone(kept)
				for _, f := range got.Facts {
					all = append(all, worldFact{Assert: strings.TrimSpace(f.Assert), Reason: f.Reason})
				}
				if kept, rejected, err = addFactsReport(*outDir, round, dedupeFacts(all)); err != nil {
					return failf("bridge", err)
				}
			}
		}
	}
	report, _ := json.MarshalIndent(map[string]any{"backend": m.Name(), "seconds": time.Since(start).Seconds(), "cost_usd": usage.CostUSD,
		"candidates": len(rev.Findings), "with_rules": len(items), "facts_proposed": len(facts), "facts_kept": kept, "facts_rejected": rejected, "world_set": setWorld}, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "bridge.json"), report, 0o644); err != nil {
		return failf("bridge", err)
	}
	fmt.Fprintf(out, "%d candidate(s), %d with rules on every side: %d world fact(s) proposed, %d kept; %d attribute(s) now set by the world; about $%.2f, %.0fs\nwrote %s\n",
		len(rev.Findings), len(items), len(facts), len(kept), len(setWorld), usage.CostUSD, time.Since(start).Seconds(), *outDir)
	return 0
}

const bridgeInstructions = `
# Your task: connect suspected contradictions

A reviewer suspects each candidate's statements contradict each other. The
checker can only find a contradiction if the rules meet: if they speak of
different attributes that are linked in reality but not in the vocabulary,
it sees nothing. For each candidate, give the world facts that link its
rules, so the checker can decide whether they really conflict:

- a resource things share (the orders in a shipment leave on its one truck:
  all(o in shipment.orders: o.dispatched = shipment.dispatched));
- a part and its whole (an order's items are stored in its warehouse:
  order.itemsAvailable implies order.warehouseOpen);
- what one fact implies about another (a shipped order was paid:
  order.status = shipped implies order.paid);
- two names for one fact on two entities.

Each fact must be true in every real situation by what the things are,
never because a requirement says so, and must not decide which statement
wins. Write facts in the rule language with entity roots in lower camel
case (not self), over the declared attributes and relations only. Give no
fact for a candidate whose rules already meet, or whose statements do not
really contradict.

Also list under world the attributes (Entity.attribute) in these rules that
the world decides but the vocabulary has an operation write: what an
external service or backend returns (an HTTP error, unreachable), what a
user does, what time it is. The checker lets the system choose anything an
operation writes, so it can choose "no error" and escape every rule about
the error; the world deciding it is what makes those rules meet.
`

const bridgeSchema = `{"type":"object","additionalProperties":false,"required":["facts","world"],"properties":{"facts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["assert","reason"],"properties":{"assert":{"type":"string"},"reason":{"type":"string"}}}},"world":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["attribute","reason"],"properties":{"attribute":{"type":"string"},"reason":{"type":"string"}}}}}}`

func dedupeFacts(fs []worldFact) []worldFact {
	seen := map[string]bool{}
	var out []worldFact
	for _, f := range fs {
		k := strings.Join(strings.Fields(f.Assert), " ")
		if !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	return out
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// worldSet makes each named attribute (Entity.attribute) one the world
// decides: `setBy: world` on its declaration and out of every operation's
// writes, in the vocabulary files of dir. A change that leaves the
// registry unable to load (a rule then writes a world value) is undone.
func worldSet(dir string, attrs []string) ([]string, error) {
	var done []string
	for _, q := range attrs {
		ent, name, ok := strings.Cut(q, ".")
		if !ok {
			continue
		}
		files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
		if err != nil {
			return nil, err
		}
		saved := map[string][]byte{}
		changed := false
		for _, f := range files {
			if strings.HasSuffix(f, "rules.yaml") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			docs := strings.Split(string(src), "\n---\n")
			touched := false
			for i, d := range docs {
				var node yaml.Node
				if yaml.Unmarshal([]byte(d), &node) != nil || len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
					continue
				}
				mp := node.Content[0]
				get := func(key string) *yaml.Node {
					for k := 0; k+1 < len(mp.Content); k += 2 {
						if mp.Content[k].Value == key {
							return mp.Content[k+1]
						}
					}
					return nil
				}
				hit := false
				if e := get("entity"); e != nil && e.Value == ent || get("extend") != nil && strings.TrimPrefix(get("extend").Value, "platform.") == ent {
					if as := get("attributes"); as != nil && as.Kind == yaml.MappingNode {
						for k := 0; k+1 < len(as.Content); k += 2 {
							if as.Content[k].Value != name || as.Content[k+1].Kind != yaml.MappingNode {
								continue
							}
							a := as.Content[k+1]
							has := false
							for j := 0; j+1 < len(a.Content); j += 2 {
								if a.Content[j].Value == "setBy" {
									a.Content[j+1].Value, has = "world", true
								}
							}
							if !has {
								a.Content = append(a.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "setBy"}, &yaml.Node{Kind: yaml.ScalarNode, Value: "world"})
							}
							hit = true
						}
					}
				}
				if w := get("writes"); w != nil && w.Kind == yaml.SequenceNode && get("operation") != nil {
					kept := w.Content[:0]
					for _, x := range w.Content {
						if x.Value == q {
							hit = true
							continue
						}
						kept = append(kept, x)
					}
					w.Content = kept
				}
				if !hit {
					continue
				}
				var head string
				if strings.HasPrefix(d, "#") {
					if j := strings.Index(d, "\n"); j >= 0 {
						head = d[:j+1]
					}
				}
				b, err := yaml.Marshal(&node)
				if err != nil {
					return nil, err
				}
				docs[i] = head + strings.TrimSuffix(string(b), "\n")
				touched = true
			}
			if touched {
				saved[f] = src
				if err := os.WriteFile(f, []byte(strings.Join(docs, "\n---\n")), 0o644); err != nil {
					return nil, err
				}
				changed = true
			}
		}
		if !changed {
			continue
		}
		if _, err := ir.LoadDir(dir); err != nil {
			for f, src := range saved {
				if err := os.WriteFile(f, src, 0o644); err != nil {
					return nil, err
				}
			}
			continue
		}
		done = append(done, q)
	}
	return done, nil
}
