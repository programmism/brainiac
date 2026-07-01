package density

import (
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
)

func TestDropsEmptyAndShort(t *testing.T) {
	s := New()
	for _, in := range []string{"", "   \n\t ", "too short"} {
		if got := s.Score(in); got.Decision != plugins.Drop {
			t.Errorf("Score(%q) = %v, want Drop", in, got.Decision)
		}
	}
}

func TestDropsAllStopwords(t *testing.T) {
	s := New()
	got := s.Score("the and or of to in on at it is are was for with from into over the")
	if got.Decision != plugins.Drop {
		t.Errorf("stopword filler = %v (q=%.2f), want Drop", got.Decision, got.Quality)
	}
}

func TestKeepsDenseTechnicalText(t *testing.T) {
	s := New()
	got := s.Score("OrderService writes orders to Postgres 5 times per second for durability during peak load in 2026.")
	if got.Decision != plugins.Keep {
		t.Errorf("dense technical = %v (q=%.2f), want Keep", got.Decision, got.Quality)
	}
}

func TestQualityMonotonic(t *testing.T) {
	s := New()
	rich := s.Score("The OrderService persists 1200 orders to Postgres and Kafka for durability.")
	poor := s.Score("the and or of to in on at it is for with from the and or of to in")
	if rich.Quality <= poor.Quality {
		t.Errorf("rich quality %.2f should exceed poor %.2f", rich.Quality, poor.Quality)
	}
}

func TestThresholdsConfigurable(t *testing.T) {
	// With an impossible keep threshold nothing is kept.
	s := New(WithThresholds(1.1, 1.05))
	if got := s.Score("OrderService writes to Postgres for durability."); got.Decision == plugins.Keep {
		t.Errorf("with keep=1.1, decision should not be Keep, got %v", got.Decision)
	}
}
