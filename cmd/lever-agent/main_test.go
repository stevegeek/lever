package main

import "testing"

func TestUnknownSubcommandErrors(t *testing.T) {
	if err := run([]string{"lever-agent", "bogus"}); err == nil {
		t.Fatal("unknown subcommand must error")
	}
}

func TestRunRequiresSubcommand(t *testing.T) {
	if err := run([]string{"lever-agent"}); err == nil {
		t.Fatal("missing subcommand must error")
	}
}
