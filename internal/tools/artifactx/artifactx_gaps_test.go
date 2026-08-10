package artifactx

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// artifact.read is registered as a read-risk tool.
func TestArtifactReadRiskRegistration(t *testing.T) {
	ts := Tools(Deps{Store: fakeStore{}})
	if len(ts) != 1 || ts[0].Name != "artifact.read" {
		t.Fatalf("expected one artifact.read tool, got %+v", ts)
	}
	if ts[0].Risk != domain.RiskRead {
		t.Fatalf("artifact.read risk: got %s want read", ts[0].Risk)
	}
}

// Omitting offset defaults to 0 and a short artifact reads whole at eof.
func TestArtifactDefaultOffsetZeroAtEOF(t *testing.T) {
	deps := Deps{Store: fakeStore{"a": "abcdef"}}
	m := readResult(t, deps, `{"artifactId":"a"}`)
	if m["offset"].(int) != 0 {
		t.Fatalf("default offset: %v", m["offset"])
	}
	if m["content"].(string) != "abcdef" || !m["eof"].(bool) {
		t.Fatalf("short artifact should read whole at eof: %+v", m)
	}
}
