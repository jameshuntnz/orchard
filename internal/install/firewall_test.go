package install

import (
	"context"
	"strings"
	"testing"
)

func TestAnchorRuleOrder(t *testing.T) {
	body, err := firewallStep{plan: Plan{
		UploadAddr:  "0.0.0.0:8477",
		UploadAllow: "127.0.0.1/32,192.168.64.0/18,172.16.0.0/12",
	}}.anchor()
	if err != nil {
		t.Fatal(err)
	}

	var rules []string
	for _, line := range strings.Split(body, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			rules = append(rules, line)
		}
	}

	want := []string{
		"pass in quick on lo0 proto tcp to any port 8477",
		"pass in quick proto tcp from 192.168.64.0/18 to any port 8477",
		"pass in quick proto tcp from 172.16.0.0/12 to any port 8477",
		"block drop in quick proto tcp to any port 8477",
	}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d:\n%s", len(rules), len(want), body)
	}
	// pf takes the first quick match, so a block ahead of a pass would drop the
	// traffic the pass exists for.
	for i := range want {
		if rules[i] != want[i] {
			t.Errorf("rule %d = %q, want %q", i, rules[i], want[i])
		}
	}
}

// The loopback pass is written once, as an interface rule; repeating it as a source
// CIDR would be redundant.
func TestAnchorSkipsLoopbackCIDRs(t *testing.T) {
	body, _ := firewallStep{plan: Plan{
		UploadAddr:  "0.0.0.0:8477",
		UploadAllow: "127.0.0.1/32,::1,192.168.64.0/18",
	}}.anchor()
	if strings.Contains(body, "from 127.0.0.1/32") || strings.Contains(body, "from ::1") {
		t.Errorf("loopback repeated as a source rule:\n%s", body)
	}
	if !strings.Contains(body, "on lo0") {
		t.Error("no loopback rule at all")
	}
}

// The port comes from the listener being restricted, so the rules cannot drift from
// what the service is actually bound to.
func TestAnchorUsesTheConfiguredPort(t *testing.T) {
	body, err := firewallStep{plan: Plan{UploadAddr: "0.0.0.0:9999", UploadAllow: "10.0.0.0/8"}}.anchor()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "port 9999") || strings.Contains(body, "8477") {
		t.Errorf("wrong port in rules:\n%s", body)
	}
}

func TestAnchorRejectsAnUnparseableAddress(t *testing.T) {
	if _, err := (firewallStep{plan: Plan{UploadAddr: "not-an-address"}}).anchor(); err == nil {
		t.Error("anchor accepted an address with no port")
	}
}

func TestFirewallCheckStates(t *testing.T) {
	ctx := context.Background()

	// Nothing bound, nothing to restrict.
	if got := (firewallStep{plan: Plan{}}).Check(ctx); got.Kind != KindOK {
		t.Errorf("no listener: %v (%s), want ok", got.Kind, got.Summary)
	}

	// An explicit opt-out at the application layer leaves nothing for pf to enforce
	// either — and saying "ok" to that would misrepresent an unrestricted listener.
	got := firewallStep{plan: Plan{UploadAddr: "0.0.0.0:8477", UploadAllow: "any"}}.Check(ctx)
	if got.Kind != KindUnverified {
		t.Errorf("allow=any: %v (%s), want unverified", got.Kind, got.Summary)
	}
}

// Applying rewrites a file shared with anything else on the host that uses pf, so it is
// never something a step fixes on its own.
func TestFirewallStepNeverSelfFixes(t *testing.T) {
	_, err := firewallStep{plan: Plan{UploadAddr: "0.0.0.0:8477"}}.Fix(context.Background())
	if err == nil {
		t.Fatal("Fix applied pf rules as part of an install")
	}
	if !strings.Contains(err.Error(), "orchard firewall --apply") {
		t.Errorf("error = %q, want it to name the deliberate command", err)
	}
}
