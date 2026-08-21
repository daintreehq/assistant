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

// A dynamic target may be authorized by an explicit NAME grant and by nothing
// else. The risk-class half of the union rule is the exact widening this closes:
// `allowedRiskClasses:["terminal"]` names a bounded, reviewable set of REGISTERED
// tools, but behind a dynamic invoker the same string would mean "every
// terminal-risk action the connected Daintree happens to expose", chosen by the
// model at call time and never seen by whoever approved the grant.
func TestGrantAuthorizesDynamicTargetByNameOnly(t *testing.T) {
	riskGrant := domain.AutomationGrantRecord{
		AllowedRiskClassesJson: strPtrGrants(`["terminal","read"]`),
	}
	for _, name := range []string{"daintree.invoke:terminal.new", "daintree.invoke:terminal.list"} {
		if grantAuthorizes(riskGrant, name, domain.RiskTerminal) {
			t.Errorf("a risk-class grant must NOT authorize the dynamic target %s", name)
		}
	}
	// A name-scoped grant on the resolved target does authorize it — otherwise a
	// watcher could never be given one specific MCP action.
	nameGrant := domain.AutomationGrantRecord{
		AllowedToolNamesJson: strPtrGrants(`["daintree.invoke:terminal.new"]`),
	}
	if !grantAuthorizes(nameGrant, "daintree.invoke:terminal.new", domain.RiskTerminal) {
		t.Error("a name-scoped grant must authorize its own dynamic target")
	}
	// ...and only that one. A grant for one action never spreads to a sibling.
	if grantAuthorizes(nameGrant, "daintree.invoke:terminal.kill", domain.RiskTerminal) {
		t.Error("a dynamic-target grant must not authorize a different action")
	}
	// The bare invoker stays ungrantable however it is scoped, so a grant for the
	// generic invoker can never authorize arbitrary MCP calls.
	bareGrant := domain.AutomationGrantRecord{
		AllowedToolNamesJson: strPtrGrants(`["daintree.invoke"]`),
	}
	if grantAuthorizes(bareGrant, "daintree.invoke", domain.RiskSystem) {
		t.Error("a grant naming the generic invoker must authorize nothing")
	}
}

func strPtrGrants(s string) *string { return &s }
