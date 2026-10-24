package cli

import (
	"testing"
	"time"
)

// A read running alone is exact; reads that overlap are flagged, and the
// average number running over a read's window is what its CPU is divided by.
func TestServiceReadsShareTheirOverlap(t *testing.T) {
	alone := &serviceReport{answered: true}
	beginServiceRead(alone)
	defer endServiceRead(alone)
	time.Sleep(20 * time.Millisecond)
	if concurrency, shared := readConcurrency(alone); shared || concurrency < 0.99 || concurrency > 1.01 {
		t.Fatalf("a read alone: concurrency %v, shared %v", concurrency, shared)
	}
	endServiceRead(alone)
	first := &serviceReport{answered: true}
	beginServiceRead(first)
	defer endServiceRead(first)
	time.Sleep(100 * time.Millisecond)
	second := &serviceReport{answered: true}
	beginServiceRead(second)
	defer endServiceRead(second)
	time.Sleep(100 * time.Millisecond)
	firstConcurrency, firstShared := readConcurrency(first)
	secondConcurrency, secondShared := readConcurrency(second)
	// first ran alone for half its window and with second for the other.
	if !firstShared || firstConcurrency < 1.3 || firstConcurrency > 1.7 {
		t.Fatalf("first read: concurrency %v, shared %v; want about 1.5, shared", firstConcurrency, firstShared)
	}
	if !secondShared || secondConcurrency < 1.8 || secondConcurrency > 2.01 {
		t.Fatalf("second read: concurrency %v, shared %v; want about 2, shared", secondConcurrency, secondShared)
	}
}
