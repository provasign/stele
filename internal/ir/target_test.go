package ir

import "testing"

func TestRequestTieByTheSameName(t *testing.T) {
	for src, want := range map[string]string{
		"request.number = self.number":               "(request.number = self.number)",
		"self.number = request.number":               "(request.number = self.number)",
		"request.compression = self.fileCompression": "<nil>",
		"request.number = self.plot.number":          "<nil>",
	} {
		e, err := ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		got := "<nil>"
		if b := requestTie(e); b != nil {
			got = b.String()
		}
		if got != want {
			t.Errorf("requestTie(%s) = %s, want %s", src, got, want)
		}
	}
}
