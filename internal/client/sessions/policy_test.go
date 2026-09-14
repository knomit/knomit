package sessions

import (
	"testing"
	"time"
)

func TestParsePolicy_DefaultsAndZeroRetention(t *testing.T) {
	p, err := ParsePolicy("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.DeadAfter != time.Hour || p.HiddenAfter != 3*time.Hour || p.Retention != 168*time.Hour {
		t.Fatalf("defaults: %+v", p)
	}
	p, err = ParsePolicy("30m", "2h", "0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Retention != 0 {
		t.Fatalf("zero retention must stay zero (disabled), got %v", p.Retention)
	}
	if p.DeadAfter != 30*time.Minute || p.HiddenAfter != 2*time.Hour {
		t.Fatalf("parsed: %+v", p)
	}
	if _, err := ParsePolicy("nope", "", ""); err == nil {
		t.Fatal("malformed duration must error")
	}
}

func TestParsePolicy_NonPositiveDeadHiddenFallBackToDefaults(t *testing.T) {
	p, err := ParsePolicy("0", "-1h", "168h")
	if err != nil {
		t.Fatal(err)
	}
	if p.DeadAfter != DefaultDeadAfter || p.HiddenAfter != DefaultHiddenAfter {
		t.Fatalf("a zero window would mark every session dead at once: %+v", p)
	}
}

func TestPolicy_StateAt(t *testing.T) {
	p := Policy{DeadAfter: time.Hour, HiddenAfter: 3 * time.Hour, Retention: 168 * time.Hour}
	now := time.Unix(10_000, 0)
	cases := []struct {
		ago   time.Duration
		ended bool
		want  State
	}{
		{0, false, StateLive},
		{5 * time.Minute, false, StateLive},
		{6*time.Minute + time.Second, false, StateIdle},
		{time.Hour, false, StateIdle},
		{time.Hour + time.Second, false, StateDead},
		{0, true, StateDead},
	}
	for _, c := range cases {
		if got := p.StateAt(now.Add(-c.ago), c.ended, now); got != c.want {
			t.Errorf("ago=%v ended=%v: got %s want %s", c.ago, c.ended, got, c.want)
		}
	}
}
