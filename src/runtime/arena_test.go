// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/goarch"
	"reflect"
	. "runtime"
	"runtime/debug"
	"runtime/internal/atomic"
	"testing"
	"time"
	"unsafe"
)

type smallScalar struct {
	X uintptr
}
type smallPointer struct {
	X *smallPointer
}
type smallPointerMix struct {
	A *smallPointer
	B byte
	C *smallPointer
	D [11]byte
}
type mediumScalarEven [8192]byte
type mediumScalarOdd [3321]byte
type mediumPointerEven [1024]*smallPointer
type mediumPointerOdd [1023]*smallPointer

type largeScalar [UserArenaChunkBytes + 1]byte
type largePointer [UserArenaChunkBytes/unsafe.Sizeof(&smallPointer{}) + 1]*smallPointer

func TestUserArena(t *testing.T) {
	// Set GOMAXPROCS to 2 so we don't run too many of these
	// tests in parallel.
	defer GOMAXPROCS(GOMAXPROCS(2))

	// Start a subtest so that we can clean up after any parallel tests within.
	t.Run("Alloc", func(t *testing.T) {
		ss := &smallScalar{5}
		runSubTestUserArenaNew(t, ss, true)

		sp := &smallPointer{new(smallPointer)}
		runSubTestUserArenaNew(t, sp, true)

		spm := &smallPointerMix{sp, 5, nil, [11]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}}
		runSubTestUserArenaNew(t, spm, true)

		mse := new(mediumScalarEven)
		for i := range mse {
			mse[i] = 121
		}
		runSubTestUserArenaNew(t, mse, true)

		mso := new(mediumScalarOdd)
		for i := range mso {
			mso[i] = 122
		}
		runSubTestUserArenaNew(t, mso, true)

		mpe := new(mediumPointerEven)
		for i := range mpe {
			mpe[i] = sp
		}
		runSubTestUserArenaNew(t, mpe, true)

		mpo := new(mediumPointerOdd)
		for i := range mpo {
			mpo[i] = sp
		}
		runSubTestUserArenaNew(t, mpo, true)

		ls := new(largeScalar)
		for i := range ls {
			ls[i] = 123
		}
		// Not in parallel because we don't want to hold this large allocation live.
		runSubTestUserArenaNew(t, ls, false)

		lp := new(largePointer)
		for i := range lp {
			lp[i] = sp
		}
		// Not in parallel because we don't want to hold this large allocation live.
		runSubTestUserArenaNew(t, lp, false)

		sss := make([]smallScalar, 25)
		for i := range sss {
			sss[i] = smallScalar{12}
		}
		runSubTestUserArenaSlice(t, sss, true)

		mpos := make([]mediumPointerOdd, 5)
		for i := range mpos {
			mpos[i] = *mpo
		}
		runSubTestUserArenaSlice(t, mpos, true)

		sps := make([]smallPointer, UserArenaChunkBytes/unsafe.Sizeof(smallPointer{})+1)
		for i := range sps {
			sps[i] = *sp
		}
		// Not in parallel because we don't want to hold this large allocation live.
		runSubTestUserArenaSlice(t, sps, false)

		// Test zero-sized types.
		t.Run("struct{}", func(t *testing.T) {
			arena := NewUserArena()
			var x any
			x = (*struct{})(nil)
			arena.New(&x)
			if v := unsafe.Pointer(x.(*struct{})); v != ZeroBase {
				t.Errorf("expected zero-sized type to be allocated as zerobase: got %x, want %x", v, ZeroBase)
			}
			arena.Free()
		})
		t.Run("[]struct{}", func(t *testing.T) {
			arena := NewUserArena()
			var sl []struct{}
			arena.Slice(&sl, 10)
			if v := unsafe.Pointer(&sl[0]); v != ZeroBase {
				t.Errorf("expected zero-sized type to be allocated as zerobase: got %x, want %x", v, ZeroBase)
			}
			arena.Free()
		})
		t.Run("[]int (cap 0)", func(t *testing.T) {
			arena := NewUserArena()
			var sl []int
			arena.Slice(&sl, 0)
			if len(sl) != 0 {
				t.Errorf("expected requested zero-sized slice to still have zero length: got %x, want 0", len(sl))
			}
			arena.Free()
		})
	})

	// Run a GC cycle to get any arenas off the quarantine list.
	GC()

	if n := GlobalWaitingArenaChunks(); n != 0 {
		t.Errorf("expected zero waiting arena chunks, found %d", n)
	}
}

func runSubTestUserArenaNew[S comparable](t *testing.T, value *S, parallel bool) {
	t.Run(reflect.TypeOf(value).Elem().Name(), func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Allocate and write data, enough to exhaust the arena.
		//
		// This is an underestimate, likely leaving some space in the arena. That's a good thing,
		// because it gives us coverage of boundary cases.
		n := int(UserArenaChunkBytes / unsafe.Sizeof(*value))
		if n == 0 {
			n = 1
		}

		// Create a new arena and do a bunch of operations on it.
		arena := NewUserArena()

		arenaValues := make([]*S, 0, n)
		for j := 0; j < n; j++ {
			var x any
			x = (*S)(nil)
			arena.New(&x)
			s := x.(*S)
			*s = *value
			arenaValues = append(arenaValues, s)
		}
		// Check integrity of allocated data.
		for _, s := range arenaValues {
			if *s != *value {
				t.Errorf("failed integrity check: got %#v, want %#v", *s, *value)
			}
		}

		// Release the arena.
		arena.Free()
	})
}

func runSubTestUserArenaSlice[S comparable](t *testing.T, value []S, parallel bool) {
	t.Run("[]"+reflect.TypeOf(value).Elem().Name(), func(t *testing.T) {
		if parallel {
			t.Parallel()
		}

		// Allocate and write data, enough to exhaust the arena.
		//
		// This is an underestimate, likely leaving some space in the arena. That's a good thing,
		// because it gives us coverage of boundary cases.
		n := int(UserArenaChunkBytes / (unsafe.Sizeof(*new(S)) * uintptr(cap(value))))
		if n == 0 {
			n = 1
		}

		// Create a new arena and do a bunch of operations on it.
		arena := NewUserArena()

		arenaValues := make([][]S, 0, n)
		for j := 0; j < n; j++ {
			var sl []S
			arena.Slice(&sl, cap(value))
			copy(sl, value)
			arenaValues = append(arenaValues, sl)
		}
		// Check integrity of allocated data.
		for _, sl := range arenaValues {
			for i := range sl {
				got := sl[i]
				want := value[i]
				if got != want {
					t.Errorf("failed integrity check: got %#v, want %#v at index %d", got, want, i)
				}
			}
		}

		// Release the arena.
		arena.Free()
	})
}

func TestUserArenaLiveness(t *testing.T) {
	t.Run("Free", func(t *testing.T) {
		testUserArenaLiveness(t, false)
	})
	t.Run("Finalizer", func(t *testing.T) {
		testUserArenaLiveness(t, true)
	})
}

func testUserArenaLiveness(t *testing.T, useArenaFinalizer bool) {
	// Disable the GC so that there's zero chance we try doing anything arena related *during*
	// a mark phase, since otherwise a bunch of arenas could end up on the fault list.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	// Defensively ensure that any full arena chunks leftover from previous tests have been cleared.
	GC()
	GC()

	arena := NewUserArena()

	// Allocate a few pointer-ful but un-initialized objects so that later we can
	// place a reference to heap object at a more interesting location.
	for i := 0; i < 3; i++ {
		var x any
		x = (*mediumPointerOdd)(nil)
		arena.New(&x)
	}

	var x any
	x = (*smallPointerMix)(nil)
	arena.New(&x)
	v := x.(*smallPointerMix)

	var safeToFinalize atomic.Bool
	var finalized atomic.Bool
	v.C = new(smallPointer)
	SetFinalizer(v.C, func(_ *smallPointer) {
		if !safeToFinalize.Load() {
			t.Error("finalized arena-referenced object unexpectedly")
		}
		finalized.Store(true)
	})

	// Make sure it stays alive.
	GC()
	GC()

	// In order to ensure the object can be freed, we now need to make sure to use
	// the entire arena. Exhaust the rest of the arena.

	for i := 0; i < int(UserArenaChunkBytes/unsafe.Sizeof(mediumScalarEven{})); i++ {
		var x any
		x = (*mediumScalarEven)(nil)
		arena.New(&x)
	}

	// Make sure it stays alive again.
	GC()
	GC()

	v = nil

	safeToFinalize.Store(true)
	if useArenaFinalizer {
		arena = nil

		// Try to queue the arena finalizer.
		GC()
		GC()

		// In order for the finalizer we actually want to run to execute,
		// we need to make sure this one runs first.
		if !BlockUntilEmptyFinalizerQueue(int64(2 * time.Second)) {
			t.Fatal("finalizer queue was never emptied")
		}
	} else {
		// Free the arena explicitly.
		arena.Free()
	}

	// Try to queue the object's finalizer that we set earlier.
	GC()
	GC()

	if !BlockUntilEmptyFinalizerQueue(int64(2 * time.Second)) {
		t.Fatal("finalizer queue was never emptied")
	}
	if !finalized.Load() {
		t.Error("expected arena-referenced object to be finalized")
	}
}

func TestUserArenaClearsPointerBits(t *testing.T) {
	// This is a regression test for a serious issue wherein if pointer bits
	// aren't properly cleared, it's possible to allocate scalar data down
	// into a previously pointer-ful area, causing misinterpretation by the GC.

	// Create a large object, grab a pointer into it, and free it.
	x := new([8 << 20]byte)
	xp := uintptr(unsafe.Pointer(&x[124]))
	var finalized atomic.Bool
	SetFinalizer(x, func(_ *[8 << 20]byte) {
		finalized.Store(true)
	})

	// Write three chunks worth of pointer data. Three gives us a
	// high likelihood that when we write 2 later, we'll get the behavior
	// we want.
	a := NewUserArena()
	for i := 0; i < int(UserArenaChunkBytes/goarch.PtrSize*3); i++ {
		var x any
		x = (*smallPointer)(nil)
		a.New(&x)
	}
	a.Free()

	// Recycle the arena chunks.
	GC()
	GC()

	a = NewUserArena()
	for i := 0; i < int(UserArenaChunkBytes/goarch.PtrSize*2); i++ {
		var x any
		x = (*smallScalar)(nil)
		a.New(&x)
		v := x.(*smallScalar)
		// Write a pointer that should not keep x alive.
		*v = smallScalar{xp}
	}
	KeepAlive(x)
	x = nil

	// Try to free x.
	GC()
	GC()

	if !BlockUntilEmptyFinalizerQueue(int64(2 * time.Second)) {
		t.Fatal("finalizer queue was never emptied")
	}
	if !finalized.Load() {
		t.Fatal("heap allocation kept alive through non-pointer reference")
	}

	// Clean up the arena.
	a.Free()
	GC()
	GC()
}