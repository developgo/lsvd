package lsvd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtent(t *testing.T) {
	e := func(lba LBA, blocks uint32) Extent {
		return Extent{lba, blocks}
	}

	t.Run("covers", func(t *testing.T) {
		r := require.New(t)

		r.Equal(CoverExact, e(1, 1).Cover(e(1, 1)))

		for _, x := range []Extent{e(0, 1), e(1, 2), e(9, 1)} {
			r.Equal(CoverSuperRange, e(0, 10).Cover(x))
		}

		for _, x := range []Extent{e(9, 2), e(15, 20), e(0, 100)} {
			r.Equal(CoverPartly, e(10, 10).Cover(x))
		}

		for _, x := range []Extent{e(0, 10), e(20, 1)} {
			r.Equal(CoverNone, e(10, 10).Cover(x), "%s covers but shouldn't", x)
		}
	})

	t.Run("clamp", func(t *testing.T) {
		r := require.New(t)

		chk := func(res, lhs, rhs Extent) {
			act, ok := lhs.Clamp(rhs)
			r.True(ok, "unable to clamp")
			r.Equal(res, act)
		}

		chk(e(2, 4), e(1, 10), e(2, 4))

		chk(e(28, 5), e(1, 32), e(28, 32))

		chk(e(121667583, 1), e(121667583, 2), e(121667583, 1))
	})

	t.Run("sub", func(t *testing.T) {
		r := require.New(t)

		chk := func(lhs, rhs Extent, rest ...Extent) {
			act, ok := lhs.Sub(rhs)
			r.True(ok, "unable to sub %s - %s", lhs, rhs)
			r.Equal(rest, act)
		}

		chk(e(1, 10), e(1, 1), e(2, 9))
		chk(e(1, 10), e(2, 1), e(1, 1), e(3, 8))
		chk(e(1, 10), e(9, 2), e(1, 8))
		chk(e(1, 10), e(9, 1), e(1, 8), e(10, 1))

		chk(e(10, 10), e(8, 3), e(11, 9))
	})

	t.Run("sub_many", func(t *testing.T) {
		r := require.New(t)

		res, ok := e(0, 10).SubMany([]Extent{e(1, 1), e(2, 1), e(8, 2)})
		r.True(ok)
		r.Equal([]Extent{e(0, 1), e(3, 5)}, res)

		res, ok = e(0, 10).SubMany([]Extent{e(8, 2), e(2, 1), e(1, 1)})
		r.True(ok)
		r.Equal([]Extent{e(0, 1), e(3, 5)}, res)

		res, ok = e(0, 4).SubMany([]Extent{e(1, 1)})
		r.True(ok)
		r.Equal([]Extent{e(0, 1), e(2, 2)}, res)
	})

	t.Run("mask", func(t *testing.T) {
		r := require.New(t)

		m := Extent{0, 4}.StartMask()

		r.NoError(m.Cover(Extent{0, 1}))
		r.NoError(m.Cover(Extent{1, 19}))

		holes := m.Holes()
		r.Len(holes, 0)
	})
}
