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
