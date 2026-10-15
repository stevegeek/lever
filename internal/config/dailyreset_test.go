package config

import "testing"

func TestDailyResetLoad(t *testing.T) {
	for _, v := range []string{"06:30", "\"06:30\"", "'23:59'", "00:00"} {
		app, err := LoadNoHostChecks(writeConfig(t, claudeBase+"manager:\n  daily_reset: "+v+"\n"))
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if app.Manager.DailyReset == "" {
			t.Fatalf("%s: not read", v)
		}
	}
	app, err := LoadNoHostChecks(writeConfig(t, claudeBase+"manager: {}\n"))
	if err != nil || app.Manager.DailyReset != "" {
		t.Fatalf("unset: %q %v", app.Manager.DailyReset, err)
	}
}

func TestDailyResetRejected(t *testing.T) {
	for _, v := range []string{"6:30", "24:00", "06:60", "0630", "\"06:30 \"", "morning", "1"} {
		t.Run(v, func(t *testing.T) {
			rejectNoHost(t, claudeBase+"manager:\n  daily_reset: "+v+"\n", "manager.daily_reset")
		})
	}
}
