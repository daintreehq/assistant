package storage

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// A grant scoped to a RISK CLASS must not authorize an ungrantable tool.
//
// grant.create refuses an ungrantable tool by NAME, but it accepts allowedRiskClasses,
// and the authorization rule is "toolName OR riskClass" — so a grant scoped to `system`
// matched daintree.call, the raw unbounded MCP escape hatch. That let a watcher, timer,
// or unattended wake turn reach any Daintree MCP method with no human present, which is
// precisely what the ungrantable list exists to prevent.
func TestGrantAuthorizesRefusesUngrantableToolsByRiskClass(t *testing.T) {
	riskGrant := domain.AutomationGrantRecord{
		AllowedRiskClassesJson: strPtrGrants(`["system"]`),
	}
	if grantAuthorizes(riskGrant, "daintree.call", domain.RiskSystem) {
		t.Error("a system risk-class grant must NOT authorize daintree.call")
	}
	if grantAuthorizes(riskGrant, "grant.create", domain.RiskSystem) {
		t.Error("a system risk-class grant must NOT authorize grant.create")
	}
	if grantAuthorizes(riskGrant, "grant.revoke", domain.RiskSystem) {
		t.Error("a system risk-class grant must NOT authorize grant.revoke")
	}
	// The rule is scoped to ungrantable names only — an ordinary system-risk tool is
	// still authorized, or the risk-class scope would be useless.
	if !grantAuthorizes(riskGrant, "some.other.systemTool", domain.RiskSystem) {
		t.Error("a system risk-class grant must still authorize an ordinary system tool")
	}
	// And a name-scoped grant cannot smuggle one in either.
	nameGrant := domain.AutomationGrantRecord{
		AllowedToolNamesJson: strPtrGrants(`["daintree.call"]`),
	}
	if grantAuthorizes(nameGrant, "daintree.call", domain.RiskSystem) {
		t.Error("a name-scoped grant must NOT authorize daintree.call")
	}
}

func strPtrGrants(s string) *string { return &s }
