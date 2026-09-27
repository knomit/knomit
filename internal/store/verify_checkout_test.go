package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVerifyCheckout: `knomit verify ci` over a plain bare repository. The
// verdict is the fold's, restricted to the commits the candidate adds.
func TestVerifyCheckout(t *testing.T) {
	o := newOriginFixture(t)
	root := o.commit(o.baseFiles, nil)
	files := with(o.baseFiles, o.ontPath, o.ont(VerifyEnforce, o.a, o.b))
	e := o.commit(files, o.op, root)
	good := o.commit(with(files, "kb/g.md", "g"), o.a, e)
	o.setBranch("main", good)

	fine := o.commit(with(files, "kb/g.md", "g", "kb/f.md", "f"), o.b, good)
	o.setBranch("agent/fine", fine)
	bad := o.commit(with(files, "kb/g.md", "g", "kb/x.md", "x"), o.stranger, good)
	o.setBranch("agent/bad", bad)
	// The candidate switches verification off in its OWN ontology, unsigned,
	// then adds garbage: the head's file says off, the fold still enforces.
	offFiles := with(files, "kb/g.md", "g", o.ontPath, o.ont(VerifyOff))
	relax := o.commit(offFiles, nil, good)
	garbage := o.commit(with(offFiles, "kb/y.md", "y"), o.stranger, relax)
	o.setBranch("agent/relax", garbage)

	v, err := VerifyCheckout(o.bare, "main", "agent/fine", o.root, nil)
	require.NoError(t, err)
	require.False(t, v.Blocked(), "%+v", v)
	require.Equal(t, 1, v.New)

	v, err = VerifyCheckout(o.bare, "main", "agent/bad", o.root, nil)
	require.NoError(t, err)
	require.True(t, v.Blocked())
	require.Len(t, v.Refused, 1)
	require.Equal(t, bad.Hash.String(), v.Refused[0].Commit)

	v, err = VerifyCheckout(o.bare, "main", "agent/relax", o.root, nil)
	require.NoError(t, err)
	require.Equal(t, VerifyEnforce, v.Mode, "the mode is the fold's, never the candidate head's file (which says off)")
	require.True(t, v.Blocked())
	require.Len(t, v.Refused, 2)
}

// TestVerifyCheckout_OffAndUnrooted: an off repository checks nothing; a job
// without the operator key cannot judge an enable in the candidate and blocks.
func TestVerifyCheckout_OffAndUnrooted(t *testing.T) {
	o := newOriginFixture(t)
	root := o.commit(o.baseFiles, nil)
	o.setBranch("main", root)
	x := o.commit(with(o.baseFiles, "kb/x.md", "x"), o.stranger, root)
	o.setBranch("agent/x", x)
	v, err := VerifyCheckout(o.bare, "main", "agent/x", o.root, nil)
	require.NoError(t, err)
	require.False(t, v.Blocked(), "an off repository checks nothing")

	en := o.commit(with(o.baseFiles, o.ontPath, o.ont(VerifyLog, o.a)), o.op, root)
	o.setBranch("agent/enable", en)
	v, err = VerifyCheckout(o.bare, "main", "agent/enable", StaticRoot{}, nil)
	require.NoError(t, err)
	require.True(t, v.Unrooted)
	require.True(t, v.Blocked(), "without the operator key the job cannot judge the enable")
}

// TestVerifyCheckout_BlocksNotedPolicyChange: a policy change the fold only
// NOTES (log keeps the old context and reports; off reports an unauthorised
// enable) still blocks the forge check. Merging it would leave main's file
// saying one mode while every instance folds another.
func TestVerifyCheckout_BlocksNotedPolicyChange(t *testing.T) {
	o := newOriginFixture(t)
	root := o.commit(o.baseFiles, nil)
	files := with(o.baseFiles, o.ontPath, o.ont(VerifyLog, o.a))
	en := o.commit(files, o.op, root)
	o.setBranch("main", en)

	relax := o.commit(with(files, o.ontPath, o.ont(VerifyOff)), o.a, en)
	o.setBranch("agent/relax", relax)
	v, err := VerifyCheckout(o.bare, "main", "agent/relax", o.root, nil)
	require.NoError(t, err)
	require.Equal(t, VerifyLog, v.Mode)
	require.Empty(t, v.Refused, "log refuses nothing")
	require.Len(t, v.Reported, 1)
	require.Equal(t, RulePolicyChange, v.Reported[0].Rule)
	require.True(t, v.Blocked(), "a noted relaxation blocks the forge check")

	o.setBranch("off", root)
	enable := o.commit(with(o.baseFiles, o.ontPath, o.ont(VerifyEnforce, o.stranger)), o.stranger, root)
	o.setBranch("agent/enable", enable)
	v, err = VerifyCheckout(o.bare, "off", "agent/enable", o.root, nil)
	require.NoError(t, err)
	require.Equal(t, VerifyOff, v.Mode, "a stranger cannot enable")
	require.True(t, v.Blocked(), "an unauthorised enable blocks even in an off repository: %+v", v)

}
