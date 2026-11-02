package cli

import (
	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
)

// situation renders a counterexample as values a spec author reads.
func situation(reg *ir.Registry, bs []checker.Binding) []string {
	return checker.Situation(reg, bs)
}
