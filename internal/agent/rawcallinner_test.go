package agent

import "testing"

// Both invokers can open or destroy terminals, and the roster cache is
// invalidated by matching the INNER action name against "terminal.". They spell
// that name differently — daintree.call uses `name`, daintree.invoke uses
// `action` — so a reader that knew only one spelling left every dynamic terminal
// mutation silently stale: a new terminal missing from the next round's roster,
// and the model reasoning about a world one mutation out of date.
func TestRawCallInnerNameReadsBothInvokerSpellings(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"daintree.call spelling", `{"name":"terminal.kill","arguments":{"terminalId":"t1"}}`, "terminal.kill"},
		{"daintree.invoke spelling", `{"action":"terminal.new","arguments":{"cwd":"/tmp"}}`, "terminal.new"},
		// `name` wins when both are present, so an `action` key nested in a raw call's
		// own arguments can never shadow the real target. (Only top-level fields are
		// read, so a nested one is invisible here anyway — this pins both facts.)
		{"name wins over action", `{"name":"terminal.kill","action":"not.this"}`, "terminal.kill"},
		{"nested action ignored", `{"name":"recipe.run","arguments":{"action":"terminal.new"}}`, "recipe.run"},
		{"padding trimmed", `{"action":"  terminal.new  "}`, "terminal.new"},
		{"neither present", `{"arguments":{"x":1}}`, ""},
		{"malformed json", `not json`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawCallInnerName(tc.raw); got != tc.want {
				t.Errorf("rawCallInnerName(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
