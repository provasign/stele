package llm

import "context"

// Cacher is a client whose backend caches the system prompt: a request
// with the same system prompt as an earlier one reads it from the cache,
// for a tenth of the price of writing it.
type Cacher interface{ CachesPrompts() bool }

// CachesPrompts reports whether m's backend caches the system prompt.
func CachesPrompts(m Client) bool {
	c, ok := m.(Cacher)
	return ok && c.CachesPrompts()
}

func (*claudeCLI) CachesPrompts() bool    { return true }
func (*codexCLI) CachesPrompts() bool     { return true } // a prompt's start, the system part first
func (*apiClient) CachesPrompts() bool    { return true }
func (*openAIClient) CachesPrompts() bool { return true }

// warmMin is the smallest system prompt worth a warm-up call: below it,
// the call's own reply costs about what the cache saves.
const warmMin = 8000

// Warm prepares n requests that share req.System and are about to be sent
// at once. Sent together, each writes the system prompt to the cache, as
// none is there yet: on a 60-requirement run every call of a translation
// wave and of the synonym check wrote its whole prompt (2026-11-06). Warm
// sends req first, a request that asks for nothing, so that the n read it
// instead. It does nothing when n is below 2, the backend caches nothing
// or the system prompt is short. The reply is returned for its usage, or
// nil; a failed warm-up is no error: the requests then write the cache
// themselves.
func Warm(ctx context.Context, m Client, n int, req Request) *Reply {
	if n < 2 || len(req.System) < warmMin || !CachesPrompts(m) {
		return nil
	}
	r, err := m.Complete(ctx, req)
	if err != nil {
		return nil
	}
	return r
}
