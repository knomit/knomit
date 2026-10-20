package store

import (
	"bytes"
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

const hostV1 = "# knomit fleet host v1\nurl: https://hub.example:7443/git/fleet\n"

// judgeEntry judges c as judgeIncoming does (a member's branch entering the
// fleet repository): the fleet and the ontology at the fixture's main, the
// fleet repository judging itself, entry set.
func (f *rangeFixture) judgeEntry(c *object.Commit) (Refusal, bool) {
	f.t.Helper()
	fleet, err := LoadFleetAt(f.fleetDir, "main", "")
	require.NoError(f.t, err)
	ref, err := f.fleet.Storer.Reference(plumbing.NewBranchReferenceName("main"))
	require.NoError(f.t, err)
	mainC, err := object.GetCommit(f.fleet.Storer, ref.Hash())
	require.NoError(f.t, err)
	raw, err := TreeOntology(mainC)
	require.NoError(f.t, err)
	ont, err := fact.ParseOntology(raw)
	require.NoError(f.t, err)
	gate := newJudge(fleet, "kb", ont, "")
	gate.entry = true
	return gate.judgeCommit(c)
}

// The host-record rule: on the fleet repository no commit of a member's
// branch may change artifacts/fleet/host or artifacts/fleet/master, which the fleet ontology DECLARES
// (so the record-owner rule exempts it) and only the fleet's master writes.
// Every refused row is signed by agent-x, an active member with a valid
// leaf, or by a joiner whose self-registration rule A would otherwise admit,
// so only this rule can refuse; the control rows are admitted. The row count
// is asserted.
//
// SABOTAGE: make hostRecord return ok unconditionally -> every host-record
// row is admitted (the declared path passes the owner rule) -> red; compare
// the path exactly instead of in any case -> the HOST row is admitted -> red;
// drop the FleetMasterPath check -> the master-marker rows are admitted -> red.
func TestHostRecord_AMemberBranchNeverWritesIt(t *testing.T) {
	const own = "kb/members/agent-x/1.md"
	x, j := namedSigner(t, "x"), namedSigner(t, "joiner")
	evil := "# knomit fleet host v1\nurl: https://evil.example/git/fleet\n"
	rows := []struct {
		name  string
		host  bool // main already holds a host record
		files func(b map[string]string) map[string]string
		by    string // "agent-x" or "joiner"
		want  string // "" = admitted
	}{
		{"edits its own record (control)", true, func(b map[string]string) map[string]string { return with(b, own, b[own]+"\nv2\n") }, "agent-x", ""},
		{"adds the host record", false, func(b map[string]string) map[string]string { return with(b, fact.FleetHostPath, hostV1) }, "agent-x", RuleHostRecord},
		{"rewrites the host record", true, func(b map[string]string) map[string]string { return with(b, fact.FleetHostPath, evil) }, "agent-x", RuleHostRecord},
		{"deletes the host record", true, func(b map[string]string) map[string]string { return without(b, fact.FleetHostPath) }, "agent-x", RuleHostRecord},
		{"adds it in another case spelling", false, func(b map[string]string) map[string]string { return with(b, "artifacts/fleet/HOST", hostV1) }, "agent-x", RuleHostRecord},
		// The master marker (fact.FleetMasterPath) is guarded by the same rule.
		{"names itself the acting master", false, func(b map[string]string) map[string]string { return with(b, fact.FleetMasterPath, "agent-x\n") }, "agent-x", RuleHostRecord},
		{"rewrites the master marker", true, func(b map[string]string) map[string]string { return with(b, fact.FleetMasterPath, "agent-x\n") }, "agent-x", RuleHostRecord},
		{"deletes the master marker", true, func(b map[string]string) map[string]string { return without(b, fact.FleetMasterPath) }, "agent-x", RuleHostRecord},
		{"a self-registration that also writes it", false, func(b map[string]string) map[string]string {
			return with(b, "kb/members/joiner/1.md", memberFile("joiner", fact.MemberActive, j.PublicKey()), fact.FleetHostPath, hostV1)
		}, "joiner", RuleHostRecord},
		{"a self-registration alone (control)", false, func(b map[string]string) map[string]string {
			return with(b, "kb/members/joiner/1.md", memberFile("joiner", fact.MemberActive, j.PublicKey()))
		}, "joiner", ""},
	}
	ran := 0
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			p := newTestFleetPKI(t)
			boss := namedSigner(t, "boss")
			bl, _ := p.leaf("boss", boss)
			xl, _ := p.leaf("agent-x", x)
			jl, _ := p.leaf("joiner", j)
			arts := p.artifacts(p.crl(), map[string]string{"boss": bl, "agent-x": xl, "joiner": jl})
			arts[own] = memberFile("agent-x", fact.MemberActive, x.PublicKey())
			if row.host {
				arts[fact.FleetHostPath] = hostV1
				arts[fact.FleetMasterPath] = "boss\n"
			}
			f, main, base := selfRegFixture(t, arts)
			signer := x
			if row.by == "joiner" {
				signer = j
			}
			r, ok := f.judgeEntry(f.commit(f.fleet, row.files(base), row.by, signer, main))
			if row.want == "" {
				require.True(t, ok, "%+v", r)
			} else {
				require.False(t, ok, "admitted")
				require.Equal(t, row.want, r.Rule, r.Reason)
				require.Contains(t, r.Reason, "only the fleet's master writes")
			}
			ran++
		})
	}
	require.Equal(t, 10, ran)
}

// A member's clean merge of main, where the master changed the host record,
// adds nothing beyond its parents (M3) and is admitted, although against its
// own side it changes the host record; the same merge that also rewrites the
// host record is host-record.
//
// SABOTAGE: run hostRecord before the M3 shortcut -> the clean merge is
// host-record -> red; diff against the first parent only (parents[:1]) ->
// the reverting merge is admitted -> red.
func TestHostRecord_AMemberMergesTheMastersChange(t *testing.T) {
	f, main, base, boss, x := ownerFixture(t)
	const own = "kb/members/agent-x/1.md"
	side := f.commit(f.fleet, with(base, own, base[own]+"\nv2\n"), "agent-x", x, main)
	hosted := with(base, fact.FleetHostPath, hostV1)
	main2 := f.commit(f.fleet, hosted, "boss", boss, main)
	f.ref(f.fleet, "main", main2)
	merged := with(hosted, own, base[own]+"\nv2\n")

	r, ok := f.judgeEntry(f.commit(f.fleet, merged, "agent-x", x, side, main2))
	require.True(t, ok, "the clean merge: %+v", r)

	dirty := f.commit(f.fleet, with(merged, fact.FleetHostPath, "# knomit fleet host v1\nurl: https://evil.example/git/fleet\n"), "agent-x", x, side, main2)
	r, ok = f.judgeEntry(dirty)
	require.False(t, ok)
	require.Equal(t, RuleHostRecord, r.Rule)

	// A merge of main that REVERTS the master's change: it keeps its own
	// side's tree (no host record) plus one more edit of the member's own
	// record, so it is not a clean merge, and against its first parent (side)
	// the host record does not change at all. Only the diff against the
	// second parent (main2, which added it) shows the delete.
	reverted := f.commit(f.fleet, with(base, own, base[own]+"\nv2\nv3\n"), "agent-x", x, side, main2)
	r, ok = f.judgeEntry(reverted)
	require.False(t, ok, "a merge that reverts the master's host record is admitted")
	require.Equal(t, RuleHostRecord, r.Rule, r.Reason)
}

// On a knomit host the push of a member's branch that writes the host record
// is refused host-record (judgeIncoming sets entry); CheckRange over the very
// same commit admits it, because the gate judges any range, the master's own
// create commits included, and the rule is the member branch's.
//
// SABOTAGE: drop `gate.entry = true` from judgeIncoming -> the push is
// admitted -> red.
func TestHostRecord_ReceivePackRefusesCheckRangeAdmits(t *testing.T) {
	p := newTestFleetPKI(t)
	r := namedSigner(t, "rogue")
	rl, _ := p.leaf("rogue", r)
	extra := p.artifacts(p.crl(), map[string]string{"rogue": rl})
	extra["kb/members/rogue/1.md"] = memberFile("rogue", fact.MemberActive, r.PublicKey())
	h := newFleetPushHost(t, extra)
	fp, _ := pusherOf(t, r)
	h.setPolicy(PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: func(context.Context) (Fleet, error) {
		return h.svc.FleetAt("main")
	}})

	peer, dir := newDiskPeer(t, h.srv.URL)
	head := peer.head()
	c := peer.commitAs("rogue", []*object.Commit{head}, r, with(peer.files(head), fact.FleetHostPath, hostV1))
	peer.setBranch("work", c)
	err := peer.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/agent/rogue")
	require.Error(t, err)
	m := pushRuleRE.FindStringSubmatch(err.Error())
	require.NotNil(t, m, "unexpected push error: %v", err)
	require.Equal(t, RuleHostRecord, m[1])
	require.Equal(t, plumbing.ZeroHash, h.ref("agent/rogue"), "a refused branch is not created")

	v, err := CheckRange(RangeInput{KBDir: dir, Main: "refs/remotes/origin/main", Candidate: c.Hash.String(),
		FleetDir: dir, FleetRev: "refs/remotes/origin/main"})
	require.NoError(t, err)
	require.Equal(t, 1, v.Checked)
	require.Empty(t, v.Refused, "CheckRange has no member branch: the rule is not its")
}
