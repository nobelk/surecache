// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestModelQuiescence_DrainsAndBlocksAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var q modelQuiescence
		q.enter()
		// Active writers must not be serialized.
		second := make(chan struct{})
		go func() { q.enter(); close(second); q.leave() }()
		<-second

		checking := make(chan struct{})
		finish := make(chan struct{})
		done := make(chan struct{})
		go func() {
			q.withIdle(func() { close(checking); <-finish })
			close(done)
		}()
		synctest.Wait()
		select {
		case <-checking:
			t.Fatal("checked before admitted writer completed")
		default:
		}

		admitted := make(chan struct{})
		go func() { q.enter(); close(admitted); q.leave() }()
		synctest.Wait()
		select {
		case <-admitted:
			t.Fatal("writer admitted while draining")
		default:
		}
		q.leave()
		<-checking
		synctest.Wait()
		select {
		case <-admitted:
			t.Fatal("writer admitted during checks")
		default:
		}
		close(finish)
		<-done
		<-admitted
	})
}

func TestModelQuiescence_IntermediateChecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &modelRun{t: t}
		m.f = newFixture(t, concurrent())
		m.f.prime(g("g0", "o0", "k0"))
		for want := 1; want <= 2; want++ {
			done := make(chan struct{})
			go func() { m.idlePhase(time.Millisecond); close(done) }()
			synctest.Wait()
			admitted := make(chan struct{})
			go func() { m.writers.enter(); close(admitted); m.writers.leave() }()
			synctest.Wait()
			select {
			case <-admitted:
				t.Fatal("writer admitted during intermediate idle phase")
			default:
			}
			<-done
			<-admitted
			if m.quiescentChecks != want {
				t.Fatalf("checks = %d, want %d before workload ends", m.quiescentChecks, want)
			}
		}
	})
}

func TestModelQuiescence_ResumesAfterPanic(t *testing.T) {
	var q modelQuiescence
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic")
			}
		}()
		q.withIdle(func() { panic("check failed") })
	}()
	q.enter()
	q.leave()
}
