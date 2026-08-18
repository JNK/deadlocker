package dockerctl

import "testing"

// Docker reports a pull per layer, interleaved and out of order. These are the
// shapes that actually arrive, in the order they arrive in.
func TestPullAggregatorReportsOneFigureAcrossLayers(t *testing.T) {
	a := newPullAggregator()

	if p := a.record("", "Pulling from library/mysql", 0, 0); p.Percent != -1 {
		t.Fatalf("manifest chatter should not produce a percentage, got %d", p.Percent)
	}

	a.record("l1", "Pulling fs layer", 0, 0)
	a.record("l2", "Pulling fs layer", 0, 0)

	p := a.record("l1", "Downloading", 50, 100)
	if p.Phase != "downloading" || p.Percent != 50 {
		t.Fatalf("one layer half done: got phase %q percent %d", p.Phase, p.Percent)
	}

	// A second layer joins with a size of its own: the figure is over the sum,
	// not over the layer that happened to report last.
	p = a.record("l2", "Downloading", 0, 300)
	if p.Percent != 12 { // 50 of 400
		t.Fatalf("two layers: want 12%%, got %d", p.Percent)
	}
	if p.Layers != 2 {
		t.Fatalf("want 2 layers, got %d", p.Layers)
	}

	// "Download complete" carries no byte counts; the layer still has to count
	// as fully downloaded or the total never reaches 100.
	a.record("l1", "Download complete", 0, 0)
	p = a.record("l2", "Downloading", 300, 300)
	if p.Percent != 100 {
		t.Fatalf("both layers downloaded: want 100%%, got %d", p.Percent)
	}

	// Extraction is its own phase with its own denominator, so the bar does not
	// sit at 100% while the slowest half of the work happens.
	p = a.record("l1", "Extracting", 25, 100)
	if p.Phase != "extracting" || p.Percent != 25 {
		t.Fatalf("extracting: got phase %q percent %d", p.Phase, p.Percent)
	}

	p = a.record("l1", "Pull complete", 0, 0)
	if p.Complete != 1 {
		t.Fatalf("want 1 completed layer, got %d", p.Complete)
	}
}

// A layer already on disk reports "Already exists" and nothing else, which must
// not leave the pull looking stalled at an unknown percentage forever.
func TestPullAggregatorCountsExistingLayers(t *testing.T) {
	a := newPullAggregator()
	p := a.record("l1", "Already exists", 0, 0)
	if p.Complete != 1 {
		t.Fatalf("want 1 complete, got %d", p.Complete)
	}
	if p.Percent != -1 {
		t.Fatalf("nothing to measure yet, want -1, got %d", p.Percent)
	}
}

func TestPullProgressNeverExceedsWhole(t *testing.T) {
	a := newPullAggregator()
	// Docker occasionally reports current above total on the final chunk.
	p := a.record("l1", "Downloading", 120, 100)
	if p.Percent != 100 {
		t.Fatalf("want the figure clamped to 100, got %d", p.Percent)
	}
}
