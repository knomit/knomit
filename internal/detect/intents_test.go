package detect

import (
	"strings"
	"testing"
)

func TestCodeIntents_Loads(t *testing.T) {
	is := CodeIntents()
	wantIntents := []string{"correction", "discovery", "decision", "fix-bug", "gotcha"}
	for _, name := range wantIntents {
		if _, ok := is.Intents[name]; !ok {
			t.Errorf("intent %q missing from CodeIntents()", name)
		}
	}
	if is.Thresholds.IntentScore <= 0 || is.Thresholds.IntentScore >= 1 {
		t.Errorf("IntentScore threshold = %v, want in (0,1)", is.Thresholds.IntentScore)
	}
}

func TestCodeIntents_CanonicalPhrasesPresent(t *testing.T) {
	is := CodeIntents()
	for name, intent := range is.Intents {
		if len(intent.CanonicalPhrases) == 0 {
			t.Errorf("intent %q has no canonical phrases", name)
		}
	}
}

func TestIntentsByProfile(t *testing.T) {
	_, err := IntentsByProfile("code")
	if err != nil {
		t.Errorf("IntentsByProfile(code) = %v, want nil", err)
	}
	_, err = IntentsByProfile("chat")
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("IntentsByProfile(chat) = %v, want unknown-profile error", err)
	}
}

func TestParseIntents_RejectsEmpty(t *testing.T) {
	_, err := Parse([]byte("thresholds:\n  intent_score: 0.5\n"))
	if err == nil {
		t.Fatal("Parse with no intents = nil, want error")
	}
}
